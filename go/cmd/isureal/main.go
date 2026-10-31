package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"example.com/isureal-company/isureal"
	"github.com/labstack/echo/v5"
)

func main() {
	if os.Getenv("ISUREAL_PPROF") == "1" {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		listener, err := net.Listen("tcp", "127.0.0.1:6060")
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			log.Printf("pprof listening on %s", listener.Addr())
			if err := http.Serve(listener, mux); err != nil {
				log.Printf("pprof server: %v", err)
			}
		}()
	}
	config := isureal.ConfigFromEnv()
	server, cleanup := isureal.NewServerWithCleanup(config)
	defer cleanup()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := (echo.StartConfig{Address: ":8080"}).Start(ctx, server); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
