package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"relayforge/internal/console"
	"relayforge/internal/process"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if p := os.Getenv("PROFILE"); p != "" && p != "development" {
		log.Error("console currently supports PROFILE=development only")
		return 1
	}
	address := os.Getenv("CONSOLE_ADDR")
	if address == "" {
		address = "127.0.0.1:8091"
	}
	upstream := os.Getenv("CONSOLE_API_URL")
	if upstream == "" {
		upstream = "http://127.0.0.1:8080"
	}
	handler, err := console.New(address, upstream)
	if err != nil {
		log.Error("invalid console configuration", "error", err.Error())
		return 1
	}
	defer handler.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Error("console listen failed")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Info("console started", "url", "http://"+address)
	if process.Serve(ctx, server, listener, 15*time.Second) != nil {
		log.Error("console stopped with error")
		return 1
	}
	return 0
}
