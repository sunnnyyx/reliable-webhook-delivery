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
	public := os.Getenv("PUBLIC_URL")
	port := os.Getenv("PORT")
	address := os.Getenv("DEMO_ADDR")
	if address == "" {
		address = "127.0.0.1:8091"
	}
	var listener net.Listener
	var err error
	if port != "" {
		if public == "" {
			log.Error("PUBLIC_URL is required for hosted demo")
			return 1
		}
		listener, err = net.Listen("tcp", net.JoinHostPort("", port))
		address = "127.0.0.1:8091"
	} else {
		listener, err = net.Listen("tcp", address)
	}
	if err != nil {
		log.Error("demo port unavailable")
		return 1
	}
	defer listener.Close()
	handler, err := console.NewDemo(address, public)
	if err != nil {
		log.Error("invalid demo configuration")
		return 1
	}
	defer handler.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Info("temporary demo started", "address", listener.Addr().String())
	if process.Serve(ctx, server, listener, 10*time.Second) != nil {
		return 1
	}
	return 0
}
