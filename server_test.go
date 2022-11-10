package server_test

import (
	"net/http"
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
		}),
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
