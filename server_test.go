package server_test

import (
	"net/http"

	"flamingo.me/server"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func ExampleRun() {
	ctx := context.Background()

	http.HandleFunc("/foo", func(w http.ResponseWriter, r *http.Request) {})
	http.HandleFunc("/bar", func(w http.ResponseWriter, r *http.Request) {})

	mux := http.NewServeMux()
	mux.HandleFunc("/mux/foo", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/mux/bar", func(w http.ResponseWriter, r *http.Request) {})

	server.Run(
		ctx,
		server.GrpcServlet(server.TcpListener(":50051"), func(server *grpc.Server) error {
			return nil
		}),
		server.HttpServlet(":8080", nil),
		server.HttpServlet(":8081", mux),
		server.HttpHealthcheckServlet(":18080"),
	)
}
