# Go Seamless Restart
[![godoc](http://img.shields.io/badge/godoc-reference-blue.svg?style=flat)](https://godoc.org/github.com/rs/seamless) [![license](http://img.shields.io/badge/license-MIT-red.svg?style=flat)](https://raw.githubusercontent.com/rs/seamless/master/LICENSE)

Package seamless implements a seamless restart strategy for daemons monitored by a service supervisor expecting non-forking daemons like daemontools, runit, systemd etc.

The seamless strategy is to fully rely on the service supervisor to restart the daemon, while providing to the daemon the full control of the restart process. To achieve this, seamless duplicates the daemon at startup in order to establish a supervisor -> launcher -> daemon relationship. The launcher is the first generation of the daemon hijacked by seamless to act as a circuit breaker between the supervisor and the supervised process.

This way, when the supervisor sends a `TERM` signal to stop the daemon, the launcher intercepts the signal and send an USR2 signal to its child (the actual daemon). In the daemon, seamless intercepts the USR2 signals to initiate the first stage of the seamless restart.

During the first stage, the daemon prepare itself to welcome a new version of itself by creating a PID file (see below) and by for instance closing file descriptors. At this point, the daemon is still supposed to accept requests. Once read, seamless make it send a `CHLD` signal back to the launcher (its parent). Upon reception, the launcher, immediately die, cutting to link between the supervisor and the daemon, making the supervisor attempting a restart of the daemon while current daemon is still running, detached and unsupervised.

Once the supervisor restarted the daemon, the daemon can start serving traffic in place of the old (still running) daemon by rebinding sockets using `SO_REUSEPORT` for instance (see different strategies in examples/). This is the second stage of the seamless restart. When ready, the new daemon calls seamless.Started which will look for a PID file, and if found, will send a `TERM` signal to the old daemon using the PID found in this file.

When the old daemon receives this `TERM` signal, the third and last stage of the seamless restart is engaged. The OnShutdown function is called so the daemon can gracefully shutdown using Go 1.8 http graceful Shutdown method for instance. This stage can last as long as you decide. When done, the old process can exit in order to conclude the seamless restart.

Seamless does not try to implement the actual graceful shutdown or to manage sockets migration. This task is left to the caller. See the examples directory for different implementations.

## Usage

Here is an example of seamless restart of an HTTP server using Go 1.8 provided graceful shutdown feature + the `SO_REUSEPORT` sockopt.

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	reuseport "github.com/kavu/go_reuseport"
	"github.com/rs/seamless"
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
	// Use github.com/kavu/go_reuseport waiting for
	// https://github.com/golang/go/issues/9661 to be fixed.
	//
	// The idea of SO_REUSEPORT flag is that two processes can listen on the
	// same host:port. Using the capability, the new daemon can listen while
	// the old daemon is still bound, allowing seemless transition from one
	// process to the other.
	l, err := reuseport.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}

	s := &http.Server{
		Addr: *listen,
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
```

Lets test this using daemontools. We first create the service directory:

    mkdir -p service
    cat <<EOF > service/run
    #!/bin/sh
    exec ./reuseport
    EOF
    chmod 755 service/run
    go build -o service/reuseport examples/reuseport

Then in a separate terminal, run supervise on this service:

    supervise ./service/

Then in two terminals, run two loops, one with fast quick request and another with artificially slow requests:

    # term 1
    while true; do curl http://localhost:8080 || break; done

    # term 2
    while true; do curl 'http://localhost:8080?delay=10s' || break; done

Then in yet another terminal, try to restart the service:

    svc -t ./service/

You should see no refused connection on the first terminal and the ongoing slow request should not be interrupted on the other one.

# License

All source code is licensed under the [MIT License](https://raw.githubusercontent.com/rs/seamless/master/LICENSE).


