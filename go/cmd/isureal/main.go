package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"example.com/isureal-company/isureal"
	"github.com/labstack/echo/v5"
)

func main() {
	config := isureal.ConfigFromEnv()
	server, cleanup := isureal.NewServerWithCleanup(config)
	defer cleanup()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := (echo.StartConfig{Address: ":8080"}).Start(ctx, server); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
