package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
)

// Servlet is a function which is supposed to run forever and graceful stop once the gracefulStop channel is closed
// any return (either error or no error) let's the server graceful stop all other servlets, and finally close
type Servlet func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error

// Listener function return either a net.Listener or an error, for use with e.g. grpcServers
type Listener func() (net.Listener, error)

// TcpListener creates a net.Listener on a tcp port
func TcpListener(addr string) Listener {
	return func() (net.Listener, error) {
		return net.Listen("tcp", addr)
	}
}

// GrpcServerServlet runs a grpc.Server instance with a given listener
func GrpcServerServlet(listener Listener, server *grpc.Server) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		socket, err := listener()
		if err != nil {
			return fmt.Errorf("[server] unable to start grpc listener: %w", err)
		}

		log.Printf("[server] grpc listening on %s", socket.Addr().String())

		go func() {
			<-gracefulStop
			log.Printf("[server] grpc graceful stopping on %s", socket.Addr().String())
			server.GracefulStop()
		}()

		go func() {
			<-ctx.Done()
			log.Printf("[server] grpc cancelling on %s", socket.Addr().String())
			server.Stop()
		}()

		close(ready)

		return server.Serve(socket)
	}
}

// GrpcServlet provides a setup grpc server for usage with grpc services
func GrpcServlet(listener Listener, configure func(server *grpc.Server) error) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		server := grpc.NewServer()
		if err := configure(server); err != nil {
			return fmt.Errorf("unable to initialize grpc server: %w", err)
		}

		return GrpcServerServlet(listener, server)(ctx, ready, gracefulStop)
	}
}

// HttpServerServlet provides a way to run an HTTP server as a servlet
func HttpServerServlet(server *http.Server) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		go func() {
			<-gracefulStop
			log.Printf("[server] http graceful stopping on %s", server.Addr)
			_ = server.Shutdown(ctx)
		}()

		go func() {
			<-ctx.Done()
			log.Printf("[server] http cancelling on %s", server.Addr)
			server.Close()
		}()

		log.Printf("[server] http listening on %s", server.Addr)

		close(ready)

		return server.ListenAndServe()
	}
}

// HttpServlet configures and runs a http server with the provided handler/mux
func HttpServlet(addr string, mux http.Handler) Servlet {
	if mux == nil {
		mux = http.DefaultServeMux
	}

	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		server := &http.Server{Addr: addr, Handler: mux, BaseContext: func(l net.Listener) context.Context { return ctx }}

		return HttpServerServlet(server)(ctx, ready, gracefulStop)
	}
}

var healthReady = make(chan struct{})

// HttpHealthcheckServlet provides
//   - /health/live with an OK response, or FAIL+412 if the context was cancelled
//   - /health/ready with either an OK response, or FAIL+412 once a the graceful shutdown is requested or the context was cancelled
func HttpHealthcheckServlet(addr string) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		mux := http.NewServeMux()
		mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "OK")
		})
		mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-ctx.Done():
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprintf(w, "CANCELLED")
				return
			default:
			}
			select {
			case <-gracefulStop:
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprintf(w, "STOPPING")
				return
			default:
			}
			select {
			case <-healthReady:
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "OK")
				return
			default:
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprintf(w, "NOT READY")
				return
			}
		})

		server := &http.Server{Addr: addr, Handler: mux}

		go func() {
			<-ctx.Done()
			log.Printf("[server] http healthcheck cancelling on %s", addr)
			_ = server.Shutdown(ctx)
		}()

		log.Printf("[server] http healthcheck listening on %s", addr)

		go func() { _ = server.ListenAndServe() }()

		close(ready)

		<-gracefulStop
		return errors.New("healthcheck switching to unready")
	}
}

// SlowServlet calls the initializer, which in turn returns the actual servlet required to run the service.
// This can be useful is initialization might need time, such as waiting for an external service to be available.
// The healthcheck will report a non-ready status until all servlets are running.
func SlowServlet(initializer func() Servlet) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}) error {
		return initializer()(ctx, ready, gracefulStop)
	}
}

const timeout = 5 * time.Second

// Run runs a all provided servlets, until either
// - the incoming context is cancelled
// - a os.Interrupt or syscall.SIGTERM signal is received
// - one servlets fails
// once one of the events happen, Run will signal all Servlets
// to gracefully shut down, and after a timeout force-stop all Servlets.
// An interrupt/term signal during shutdown stops the whole server.
func Run(ctx context.Context, servlets ...Servlet) {
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)

	graceful := make(chan struct{})
	errChannel := make(chan error)
	doneChannel := make(chan struct{})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// start all servlets
	running := new(sync.WaitGroup)
	running.Add(len(servlets))
	// wait for running to be done
	go func() {
		running.Wait()
		close(doneChannel)
	}()

	ready := new(sync.WaitGroup)
	ready.Add(len(servlets))
	go func() {
		ready.Wait()
		close(healthReady)
	}()

	for _, servlet := range servlets {
		readyChannel := make(chan struct{})
		go func() {
			<-readyChannel
			ready.Done()
		}()
		go func(servlet Servlet) {
			if err := servlet(ctx, readyChannel, graceful); err != nil {
				errChannel <- fmt.Errorf("servlet error: %w", err)
			}
			running.Done()
		}(servlet)
	}

	// look for a reason to stop
	select {
	case signal := <-signalChannel:
		log.Printf("[server] caught signal %s, stopping servlets...", signal)
	case <-ctx.Done():
		log.Printf("[server] context cancelled: %s, stopping servlets...", ctx.Err())
	case err := <-errChannel:
		log.Printf("[server] got error: %s, stopping servlets...", err)
	}

	// drain incoming servlet errors
	go func() {
		for err := range errChannel {
			log.Printf("[server] stopping: %s", err)
		}
	}()

	// notify of graceful stop
	close(graceful)

	// catch interruption of graceful shutdown
	go func() {
		signal := <-signalChannel
		log.Printf("[server] caught signal %s, exiting", signal)
		os.Exit(1)
	}()

	// wait or timeout if servlets don't exit
	select {
	case <-time.After(timeout):
		log.Printf("[server] timeout waiting for graceful stopping, cancelling")
	case <-doneChannel:
		log.Printf("[server] all servlets stopped")
		os.Exit(0)
	}

	// cancel everything and exit
	cancel()
	os.Exit(0)
}
