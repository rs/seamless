package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rs/seamless"
)

var (
	sockPath        = flag.String("unix-socket", "/tmp/seamless.sock", "Listen unix socket")
	pidFile         = flag.String("pid-file", "/tmp/unixsocket.pid", "Seemless restart PID file")
	gracefulTimeout = flag.Duration("graceful-timeout", 60*time.Second, "Maximum duration to wait for in-flight requests")
)

func init() {
	flag.Parse()
	seamless.Init(*pidFile)
}

func main() {
	// Listen on unix socket. We first remove the socket if already exit (see
	// below).
	os.Remove(*sockPath)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: *sockPath})
	if err != nil {
		log.Fatal(err)
	}

	// Disable automatic removal of unix socket on close. With this strategy, it
	// is the new process which is responsible for cleaning up the socket. This
	// way, the old process can keep the previous socket available as long as
	// possible.
	l.SetUnlinkOnClose(false)

	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d := r.URL.Query().Get("delay"); d != "" {
				if delay, err := time.ParseDuration(d); err == nil {
					time.Sleep(delay)
				}
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Server pid: %d\n", os.Getpid())
		}),
	}

	// Implement the graceful shutdown that will be triggered once the new process
	// successfully rebound the socket.
	seamless.OnShutdown(func() {
		ctx, cancel := context.WithTimeout(context.Background(), *gracefulTimeout)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			log.Print("Graceful shutdown timeout, force closing")
			s.Close()
		}
	})

	go func() {
		// Give the server a second to start
		time.Sleep(time.Second)
		if err == nil {
			// Signal seamless that the daemon is started and the socket is
			// bound successfully. If a pid file is found, seamless will send
			// a signal to the old process to start its graceful shutdown
			// sequence.
			seamless.Started()
		}
	}()
	err = s.Serve(l)
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}

	// Once graceful shutdown is initiated, the Serve method is return with a
	// http.ErrServerClosed error. We must not exit until the graceful shutdown
	// is completed. The seamless.Wait method blocks until the OnShutdown callback
	// has returned.
	seamless.Wait()
}
