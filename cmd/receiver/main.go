package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"relayforge/internal/events"
	"relayforge/internal/process"
	"relayforge/internal/receiver"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/signing"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("process", "receiver")
	address := os.Getenv("RECEIVER_ADDR")
	if address == "" {
		address = "127.0.0.1:8090"
	}
	host, port, e := net.SplitHostPort(address)
	n, ne := strconv.Atoi(port)
	if e != nil || ne != nil || host != "127.0.0.1" || n < 1 || n > 65535 {
		log.Error("RECEIVER_ADDR must bind 127.0.0.1 with a valid port")
		return 1
	}
	parts := strings.Split(os.Getenv("RECEIVER_SECRETS"), ",")
	if len(parts) > 2 {
		log.Error("supply one or two RECEIVER_SECRETS")
		return 1
	}
	keys := [][]byte{}
	for _, part := range parts {
		key, e := signing.DecodeSecret(part)
		if e != nil {
			log.Error("invalid RECEIVER_SECRETS")
			return 1
		}
		keys = append(keys, key)
	}
	source := os.Getenv("RECEIVER_SOURCE")
	if !events.ValidType(source) || os.Getenv("RECEIVER_DATABASE_URL") == "" {
		log.Error("RECEIVER_SOURCE and RECEIVER_DATABASE_URL are required")
		return 1
	}
	startup, cancelStartup := context.WithTimeout(context.Background(), 5*time.Second)
	db, e := postgres.Open(startup, os.Getenv("RECEIVER_DATABASE_URL"), 10)
	cancelStartup()
	if e != nil {
		log.Error("receiver storage startup failed")
		return 1
	}
	defer db.Close()
	var ready bool
	schemaContext, cancelSchema := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSchema()
	if e = db.QueryRow(schemaContext, "SELECT to_regclass('receiver_processed_events') IS NOT NULL AND to_regclass('receiver_business_events') IS NOT NULL").Scan(&ready); e != nil || !ready {
		log.Error("receiver schema unavailable; run migrate receiver-up")
		return 1
	}
	listener, e := net.Listen("tcp", address)
	if e != nil {
		log.Error("receiver listen failed")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &http.Server{Handler: receiver.Handler(keys, log, nil, &receiver.Postgres{DB: db, Source: source}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Info("started", "address", address)
	if process.Serve(ctx, s, listener, 15*time.Second) != nil {
		log.Error("receiver server failed")
		return 1
	}
	log.Info("stopped")
	return 0
}
