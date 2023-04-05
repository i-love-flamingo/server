package server_test

import (
	"net/http"
	"testing"
	"time"

	"flamingo.me/server"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func ExampleRun() {
	ctx := context.Background()

	http.HandleFunc("/foo", func(w http.ResponseWriter, r *http.Request) {})
	http.HandleFunc("/bar", func(w http.ResponseWriter, r *http.Request) {})

	server.Run(
		ctx,
		server.GrpcServlet(server.TcpListener(":50051"), func(server *grpc.Server) error {
			return nil
		}, nil),
		server.HttpServlet(":8080", nil),
		server.SlowServlet(func() server.Servlet {
			mux := http.NewServeMux()
			mux.HandleFunc("/mux/foo", func(w http.ResponseWriter, r *http.Request) {})
			mux.HandleFunc("/mux/bar", func(w http.ResponseWriter, r *http.Request) {})

			time.Sleep(1 * time.Second) // slow initalization

			return server.HttpServlet(":8081", mux)
		}),
		server.HttpHealthcheckServlet(":18080"),
	)
}

func TestServletsServlet(t *testing.T) {
	servlet := server.ServletsServlet(func(ctx context.Context, ready chan<- struct{}, gracefulStop <-chan struct{}, errorsC chan<- error) error {
		return nil
	})
	err := servlet(context.Background(), make(chan struct{}), make(chan struct{}), make(chan error))
	if err == nil {
		t.Error("servlet ended but did not return an error")
	}
}
