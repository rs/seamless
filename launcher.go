package seamless

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// launch forks the current program with the same arguments and exit the main go
// routine to prevent the current process from executing its main logic.
//
// All signals received on the parent process (the launcher) are forwarded to
// this child process except for the TERM signal. When a TERM signal is received
// on the parent, an USR2 signal is sent to the child. At this point, the child
// is given 10 seconds to prepare to welcome a new version of the daemon in
// parallel and send back a CHLD signal. Once the CHLD signal is received, the
// launcher exit, detaching the child from the supervisor. This way the
// supervisor can immediately restart the program while the older child can
// gracefully shutdown.
//
// If the child does not send a SIGCHLD signal back within 10 seconds, the
// launcher sends a TERM signal before dying.
func launch() {
	cmd, err := os.Executable()
	if err != nil {
		LogError("Could not determin executable path", err)
		os.Exit(1)
	}
	argv := os.Args
	attrs := &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	p, err := os.StartProcess(cmd, argv, attrs)
	if err != nil {
		LogError("Could not fork", err)
		os.Exit(1)
	}

	// Execute callbacks post the daemon launch before starting signal handler
	for _, f := range onChildDaemonLaunch {
		f()
	}

	c := make(chan os.Signal, 10)
	signal.Notify(c, syscall.SIGABRT, syscall.SIGALRM, syscall.SIGBUS, syscall.SIGCHLD,
		syscall.SIGCONT, syscall.SIGFPE, syscall.SIGHUP, syscall.SIGILL, syscall.SIGINT,
		syscall.SIGIO, syscall.SIGIOT, syscall.SIGPIPE, syscall.SIGPROF, syscall.SIGQUIT,
		syscall.SIGSEGV, syscall.SIGSYS, syscall.SIGTERM, syscall.SIGTRAP, syscall.SIGTSTP,
		syscall.SIGTTIN, syscall.SIGTTOU, syscall.SIGURG, syscall.SIGUSR1, syscall.SIGUSR2,
		syscall.SIGVTALRM, syscall.SIGWINCH, syscall.SIGXCPU, syscall.SIGXFSZ)
	go func() {
		terminated := false
		timer := make(<-chan time.Time) // never firing timer
		for {
			var sig os.Signal
			select {
			case sig = <-c:
			case <-timer:
				LogError("Child timeout, terminating", nil)
				if err := p.Signal(syscall.SIGTERM); err != nil {
					LogError("Error sending TERM signal", err)
				}
			}
			switch sig {
			case syscall.SIGTERM:
				if terminated {
					continue
				}
				if err := p.Signal(syscall.SIGUSR2); err != nil {
					LogError("Could not send USR2 signal", err)
				}
				terminated = true
				// Setup a timer after which the child is sent a SIGTERM if
				// no SIGCHLD has been recieved.
				timer = time.After(10 * time.Second)
			case syscall.SIGCHLD:
				if terminated {
					os.Exit(0)
				}
			default:
				if err := p.Signal(sig); err != nil {
					LogError(fmt.Sprintf("Error forwarding %s signal", sig), err)
				}
			}
		}
	}()
	p.Wait()
	os.Exit(0)
}
