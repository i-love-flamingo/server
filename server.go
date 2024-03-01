package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/host"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	otelEnabled = false
)

// Servlet is a function which is supposed to run forever and graceful stop once the gracefulStop channel is closed
// any return (either error or no error) let's the server graceful stop all other servlets, and finally close
type Servlet func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error

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
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		socket, err := listener()
		if err != nil {
			return fmt.Errorf("unable to start grpc listener: %w", err)
		}

		slog.With("area", "server").Info("grpc listening", "addr", socket.Addr().String())

		go func() {
			<-gracefulStop
			slog.With("area", "server").Info("grpc graceful stopping", "addr", socket.Addr().String())
			server.GracefulStop()
		}()

		go func() {
			<-ctx.Done()
			slog.With("area", "server").Info("grpc server cancelled", "addr", socket.Addr().String())
			server.Stop()
		}()

		close(ready)

		return server.Serve(socket)
	}
}

func GrpcServletErrorLoggingServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
			resp, err = handler(ctx, req)
			if err != nil {
				s := status.New(codes.Unknown, "-")
				var grpcError interface {
					GRPCStatus() *status.Status
				}
				if errors.As(err, &grpcError) {
					s = grpcError.GRPCStatus()
				}
				slog.With("area", "server").InfoContext(ctx, "gRPC call error", "method", info.FullMethod, "message", s.Message(), "code", s.Code(), "error", err)
			}
			return resp, err
		}),
		grpc.ChainStreamInterceptor(func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			err := handler(srv, ss)
			if err != nil {
				s := status.New(codes.Unknown, "-")
				var grpcError interface {
					GRPCStatus() *status.Status
				}
				if errors.As(err, &grpcError) {
					s = grpcError.GRPCStatus()
				}
				slog.With("area", "server").InfoContext(ss.Context(), "gRPC stream error", "method", info.FullMethod, "message", s.Message(), "code", s.Code(), "error", err)
			}
			return err
		}),
	}
}

// GrpcServlet provides a setup grpc server for usage with grpc services
func GrpcServlet(listener Listener, configure func(server *grpc.Server) error, opts func() []grpc.ServerOption) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		if opts == nil {
			opts = func() []grpc.ServerOption { return nil }
		}
		opts := opts()
		if otelEnabled {
			opts = append([]grpc.ServerOption{grpc.ChainUnaryInterceptor(otelgrpc.UnaryServerInterceptor()), grpc.ChainStreamInterceptor(otelgrpc.StreamServerInterceptor())}, opts...)
		}
		server := grpc.NewServer(opts...)
		if err := configure(server); err != nil {
			return fmt.Errorf("unable to initialize grpc server: %w", err)
		}

		return GrpcServerServlet(listener, server)(ctx, ready, gracefulStop, errorsC)
	}
}

// HttpServerServlet provides a way to run an HTTP server as a servlet
func HttpServerServlet(server *http.Server) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		go func() {
			<-gracefulStop
			slog.With("area", "server").Info("http graceful stopping", "addr", server.Addr)
			_ = server.Shutdown(ctx)
		}()

		go func() {
			<-ctx.Done()
			slog.With("area", "server").Info("http cancelling", "addr", server.Addr)
			server.Close()
		}()

		slog.With("area", "server").Info("http listening", "addr", server.Addr)

		close(ready)

		return server.ListenAndServe()
	}
}

// HttpServlet configures and runs a http server with the provided handler/mux
func HttpServlet(addr string, mux http.Handler) Servlet {
	if mux == nil {
		mux = http.DefaultServeMux
	}

	if otelEnabled {
		mux = otelhttp.NewHandler(mux, "flamingo.me/server/"+addr)
	}

	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		server := &http.Server{Addr: addr, Handler: mux, BaseContext: func(l net.Listener) context.Context { return ctx }}

		return HttpServerServlet(server)(ctx, ready, gracefulStop, errorsC)
	}
}

var healthReady = make(chan struct{})

// HttpHealthcheckServlet provides
//   - /health/live with an OK response, or FAIL+412 if the context was cancelled
//   - /health/ready with either an OK response, or FAIL+412 once a the graceful shutdown is requested or the context was cancelled
func HttpHealthcheckServlet(addr string) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
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

		mux.Handle("/metrics", promhttp.Handler())

		server := &http.Server{Addr: addr, Handler: mux}

		go func() {
			<-ctx.Done()
			slog.With("area", "server").Info("http healthcheck cancelling", "addr", addr)
			_ = server.Shutdown(ctx)
		}()

		slog.With("area", "server").Info("http healthcheck listening", "addr", addr)

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
	return func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		return initializer()(ctx, ready, gracefulStop, errorsC)
	}
}

var errServletStopped = errors.New("Servlet stopped")

// ServletsServlet runs all provided servlets
func ServletsServlet(servlets ...Servlet) Servlet {
	return func(ctx context.Context, ready chan<- struct{}, upstreamGracefulStop <-chan struct{}, errorsC chan<- error) error {
		servletsReady := new(sync.WaitGroup)
		servletsReady.Add(len(servlets))
		done := new(sync.WaitGroup)
		done.Add(len(servlets))

		go func() {
			servletsReady.Wait()
			close(ready)
		}()

		errChannel := make(chan error)
		gracefulStop := make(chan struct{})

		for _, servlet := range servlets {
			servletReady := make(chan struct{})
			go func() {
				<-servletReady
				servletsReady.Done()
			}()
			go func(servlet Servlet) {
				if err := servlet(ctx, servletReady, gracefulStop, errorsC); err != nil {
					errChannel <- err
				} else {
					errChannel <- errServletStopped
				}
				done.Done()
			}(servlet)
		}

		var err error
		select {
		case err = <-errChannel:
			slog.With("area", "server").Info("stopping", "error", err)
		case <-upstreamGracefulStop:
			slog.With("area", "server").Info("stopping graceful")
		}
		close(gracefulStop)

		go func() {
			for err := range errChannel {
				slog.With("area", "server").Info("stopping", "error", err)
			}
		}()
		done.Wait()
		return err
	}
}

const timeout = 5 * time.Second

func RunWithOpentelemetry(ctx context.Context, resource *resource.Resource, jaegerEndpoint string, servlets ...Servlet) {
	otelEnabled = true

	opts := []tracesdk.TracerProviderOption{
		tracesdk.WithResource(resource),
	}

	if jaegerEndpoint != "" {
		exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(jaegerEndpoint)))
		if err != nil {
			slog.With("area", "server").Error(err.Error())
			os.Exit(1)
		}
		opts = append(opts, tracesdk.WithBatcher(exp))
	}

	tp := tracesdk.NewTracerProvider(opts...)

	otel.SetTracerProvider(tp)

	exporter, err := prometheus.New()
	if err != nil {
		slog.With("area", "server").Error(err.Error())
		os.Exit(1)
	}

	otel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(exporter), metric.WithResource(resource)))

	if err = host.Start(); err != nil {
		slog.With("area", "server").Error(err.Error())
		os.Exit(1)
	}

	if err = runtime.Start(); err != nil {
		slog.With("area", "server").Error(err.Error())
		os.Exit(1)
	}

	otel.SetTextMapPropagator(b3.New())

	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)

	Run(ctx, servlets...)
}

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

	rootServlet := ServletsServlet(servlets...)
	go func() {
		errChannel <- rootServlet(ctx, healthReady, graceful, errChannel)
		close(doneChannel)
	}()

	// look for a reason to stop
	select {
	case signal := <-signalChannel:
		slog.With("area", "server").WarnContext(ctx, "caught signal, stopping servlets...", "signal", signal)
	case <-ctx.Done():
		slog.With("area", "server").WarnContext(ctx, "context cancelled, stopping servlets...", "error", ctx.Err())
	case err := <-errChannel:
		slog.With("area", "server").WarnContext(ctx, "got error, stopping servlets...", "error", err)
	}

	// drain incoming servlet errors
	go func() {
		for err := range errChannel {
			slog.With("area", "server").Info("stopping", "error", err)
		}
	}()

	// notify of graceful stop
	close(graceful)

	// catch interruption of graceful shutdown
	go func() {
		signal := <-signalChannel
		slog.With("area", "server").Info("caught signal, exiting", "signal", signal)
		os.Exit(1)
	}()

	// wait or timeout if servlets don't exit
	select {
	case <-time.After(timeout):
		slog.With("area", "server").Warn("timeout waiting for graceful stopping, cancelling")
	case <-doneChannel:
		slog.With("area", "server").Info("all servlets stopped")
		os.Exit(0)
	}

	// cancel everything and exit
	cancel()
	os.Exit(0)
}
