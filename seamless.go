// Package seamless implements a seamless restart strategy for daemons monitored
// by a service supervisor expecting non-forking daemons like daemontools,
// runit, systemd etc.
//
// The seamless strategy is to fully rely on the service supervisor to restart
// the daemon, while providing to the daemon the full control of the restart
// process. To achieve this, seamless duplicates the daemon at startup in order
// to establish a supervisor -> launcher -> daemon relationship. The launcher is
// the first generation of the daemon hijacked by seamless to act as a circuit
// breaker between the supervisor and the supervised process.
//
// This way, when the supervisor sends a TERM signal to stop the daemon, the
// launcher intercepts the signal and send an USR2 signal to its child (the
// actual daemon). In the daemon, seamless intercepts the USR2 signals to
// initiate the first stage of the seamless restart.
//
// During the first stage, the daemon prepare itself to welcome a new version of
// itself by creating a PID file (see below) and by for instance closing file
// descriptors. At this point, the daemon is still supposed to accept requests.
// Once read, seamless make it send a CHLD signal back to the launcher (its
// parent). Upon reception, the launcher, immediately die, cutting to link
// between the supervisor and the daemon, making the supervisor attempting a
// restart of the daemon while current daemon is still running, detached and
// unsupervised.
//
// Once the supervisor restarted the daemon, the daemon can start serving
// traffic in place of the old (still running) daemon by rebinding sockets using
// SO_REUSEPORT for instance (see different strategies in examples/). This is
// the second stage of the seamless restart. When ready, the new daemon calls
// seamless.Started which will look for a PID file, and if found, will send a
// TERM signal to the old daemon using the PID found in this file.
//
// When the old daemon receives this TERM signal, the third and last stage of
// the seamless restart is engaged. The OnShutdown function is called so the
// daemon can gracefully shutdown using Go 1.8 http graceful Shutdown method for
// instance. This stage can last as long as you decide. When done, the old
// process can exit in order to conclude the seamless restart.
//
// Seamless does not try to implement the actual graceful shutdown or to manage
// sockets migration. This task is left to the caller. See the examples
// directory for different implementations.
package seamless

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

var (
	// LogMessage is used to log messages. The default implementation is to call
	// log.Print with the message.
	LogMessage = func(msg string) {
		log.Printf("seamless: %s", msg)
	}

	// LogError is used to log errors. The default implementation is to call
	// log.Printf with the message followed by the error.
	LogError = func(msg string, err error) {
		log.Printf("seamless: %s: %v", msg, err)
	}

	inited              bool
	disabled            bool
	doneCh              chan struct{}
	pidFilePath         string
	shutdownRequestFunc func()
	shutdownFunc        func()
)

// Init initialize seamless. This method must be called as earliest as possible
// in the program flow, before any other goroutine are scheduled. This method
// must be called from the main goroutine, either from the main method or
// preferably from the init method in the main package.
//
// The pidFile is used for signaling between the new and old generation of the
// daemon. If the pidFile is an empty string, seamless is disabled.
func Init(pidFile string) {
	if inited {
		panic("seamless.Init already called")
	}
	doneCh = make(chan struct{})
	inited = true

	if pidFile == "" {
		disabled = true
		return
	}
	pidFilePath = pidFile

	if os.Getenv("SEAMLESS") != strconv.Itoa(os.Getppid()) {
		LogMessage("Starting child process")
		if err := os.Setenv("SEAMLESS", strconv.Itoa(os.Getpid())); err != nil {
			LogError("Could set SEAMLESS environment variable", err)
			// Disable the whole system. It should let the daemon to start anyway
			// but with no seamless restart.
			disabled = true
			return
		}
		go launch()
		runtime.Goexit()
		return
	}

	go stage1()
}

// Graceful shutdown stage 1
func stage1() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR2)
	<-c
	signal.Stop(c)

	LogMessage("Shutdown requested")
	if shutdownRequestFunc != nil {
		shutdownRequestFunc()
	}
	// At this point, we are ready to inform our parent that it can start the
	// new instance.
	if p, err := os.FindProcess(os.Getppid()); err == nil {
		if err = p.Signal(syscall.SIGCHLD); err != nil {
			LogError("Could not send SIGCHLD to parent process", err)
		}
	} else {
		LogError("Could not find parent process", err)
		// If our parent is dead already, the supervisor might still restart the
		// process so we should be able to continue regardless.
	}

	stage3()
}

// Started must be called as soon as the server is started and ready to serve.
// This mean that this method must be called after a successful listen. This can
// be challenging as a listen call is blocking. See examples directory to see
// how to do that.
func Started() {
	if !inited {
		panic("called seamless.Start before seamless.Init")
	}

	if disabled {
		return
	}

	defer func() {
		if err := ioutil.WriteFile(pidFilePath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			LogError("Could not create PID file", err)
		}
	}()

	// This is stage 2 on the other (new) process.
	b, err := ioutil.ReadFile(pidFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No pid file = no old process to notify.
			return
		}
		LogError("Notification error", fmt.Errorf("cannot read PID file: %v", err))
		return
	}
	LogMessage("Notifying old process")
	if err := os.Remove(pidFilePath); err != nil {
		LogError("Could not remove old PID file", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil {
		LogError("Notification error", fmt.Errorf("invalid PID file content: %v", err))
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		if err = p.Signal(syscall.SIGTERM); err != nil {
			LogError("Could not send SIGTERM to old process", err)
		}
	} else {
		LogError("Could not find old process", err)
	}
}

func stage3() {
	// We are waiting for a TERM signal to more to the next stage (stage 3).
	LogMessage("Ready, waiting for TERM signal")

	signal.Reset(syscall.SIGTERM)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		// Trigger stage3 if no TERM received within 10 seconds.
	}
	signal.Stop(c)

	LogMessage("Graceful shutdown started")
	if shutdownFunc != nil {
		shutdownFunc()
	}
	LogMessage("Graceful shutdown completed")
	close(doneCh)
}

// OnShutdownRequest set f to be called when a graceful shutdown is requested.
// This callback is optional and can be use to release some non-production
// resources that need to be release in order for the new daemon to start
// correctly.
//
// The actual graceful shutdown should not be initiated at this stage. See
// OnShutdown for that.
func OnShutdownRequest(f func()) {
	shutdownRequestFunc = f
}

// OnShutdown set f to be called when the graceful shutdown is engaged. When f
// returns, the graceful shutdown is considered done, and seamless.Wait will
// unblock.
func OnShutdown(f func()) {
	shutdownFunc = f
}

// Wait blocks until the seamless restart is completed. This method should be
// called at the end of the main function.
func Wait() {
	<-doneCh
}
