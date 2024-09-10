package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/rs/seamless"
	"golang.org/x/sys/unix"
)

var (
	listen          = flag.String("listen", "localhost:8080", "Listen address")
	pidFile         = flag.String("pid-file", "/tmp/reuseport.pid", "Seemless restart PID file")
	gracefulTimeout = flag.Duration("graceful-timeout", 60*time.Second, "Maximum duration to wait for in-flight requests")
)

func init() {
	flag.Parse()
	seamless.Init(*pidFile)
}

func main() {
	// The idea of SO_REUSEPORT flag is that two processes can listen on the
	// same host:port. Using the capability, the new daemon can listen while
	// the old daemon is still bound, allowing seemless transition from one
	// process to the other.
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sysErr error
			err := c.Control(func(fd uintptr) {
				sysErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			})
			if err != nil {
				return err
			}
			return sysErr
		},
	}
	l, err := lc.Listen(context.TODO(), "tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}

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
