package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"relayforge/internal/config"
	"relayforge/internal/console"
	"relayforge/internal/process"
	"relayforge/internal/repository/postgres"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if p := os.Getenv("PROFILE"); p != "" && p != "development" && p != "production" {
		log.Error("invalid PROFILE")
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
	var handler *console.Server
	var err error
	if mode := os.Getenv("CONSOLE_AUTH"); mode != "" && mode != "api-key" && mode != "github" {
		log.Error("CONSOLE_AUTH must be api-key or github")
		return 1
	}
	if os.Getenv("PROFILE") == "production" || os.Getenv("CONSOLE_AUTH") == "github" {
		cfg, e := config.Load(os.Getenv)
		if e != nil {
			log.Error("invalid production configuration")
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), cfg.StartupTimeout)
		defer cancel()
		db, e := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
		if e != nil {
			log.Error("console database unavailable")
			return 1
		}
		defer db.Close()
		identity, e := console.NewIdentity(os.Getenv("SUPABASE_URL"), os.Getenv("SUPABASE_PUBLISHABLE_KEY"))
		if e != nil {
			log.Error("invalid identity configuration")
			return 1
		}
		if os.Getenv("PROFILE") == "production" {
			handler, err = console.NewHosted(address, upstream, os.Getenv("PUBLIC_URL"), identity, &postgres.Store{DB: db})
		} else {
			handler, err = console.NewLocalGitHub(address, upstream, identity, &postgres.Store{DB: db})
		}
	} else {
		handler, err = console.New(address, upstream)
	}
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
