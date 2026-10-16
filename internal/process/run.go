package process

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"relayforge/internal/config"
	"relayforge/internal/httpapi"
	"relayforge/internal/netpolicy"
	"relayforge/internal/ratelimit"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"relayforge/internal/telemetry"
	"relayforge/internal/worker"
	"syscall"
	"time"
)

func Main(role string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("process", role)
	c, e := config.Load(os.Getenv)
	if e != nil {
		log.Error("invalid configuration", "error", e.Error())
		return 1
	}
	var vault *security.Vault
	if role == "api" || role == "worker" {
		vault, e = security.NewVault(c.EncryptionKey)
		if e != nil {
			log.Error("invalid configuration", "error", e.Error())
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, c.StartupTimeout)
	db, e := postgres.Open(startup, c.DatabaseURL, c.DBMaxConns)
	cancel()
	if e != nil {
		log.Error("startup failed", "error", e.Error())
		return 1
	}
	defer db.Close()
	addr := c.APIAddr
	if role == "worker" {
		addr = c.WorkerAddr
	}
	policy, e := netpolicy.New(c.Profile, c.DevReceiverURL, nil, nil)
	if e != nil {
		log.Error("invalid network policy")
		return 1
	}
	observation, e := telemetry.New(ctx, telemetry.Config{Role: role, TraceEndpoint: c.TraceEndpoint, SampleRatio: c.TraceSampleRatio, Log: log})
	if e != nil {
		log.Error("telemetry initialization failed")
		return 1
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if observation.Shutdown(flush) != nil {
			log.Warn("telemetry shutdown incomplete", "error_code", "telemetry_unavailable")
		}
	}()
	store := &postgres.Store{DB: db, Vault: vault, Policy: policy, Telemetry: observation, MaxTenantInFlight: c.MaxTenantInFlight, MaxTenantBacklog: c.MaxTenantBacklog}
	var limiter *ratelimit.Redis
	if role == "worker" && c.RateLimitEnabled {
		limiter, e = ratelimit.New(c.RedisURL, c.RateLimitPrefix, c.TenantRate, c.TenantBurst)
		if e != nil {
			log.Error("invalid limiter configuration")
			return 1
		}
		defer limiter.Close()
		store.Limiter = limiter
		limiter.Telemetry = observation
	}
	dependencies := map[string]telemetry.Pinger{"postgres": store}
	if limiter != nil {
		dependencies["redis"] = limiter
	}
	var queue telemetry.Source
	if role == "api" {
		queue = store
	}
	if e = observation.Observe(queue, dependencies); e != nil {
		log.Error("telemetry instruments unavailable")
		return 1
	}
	operational := http.NewServeMux()
	operational.Handle("GET /metrics", observation.Handler)
	if limiter != nil {
		operational.Handle("/health/", httpapi.Health(store, c.ReadyTimeout, limiter))
	} else {
		operational.Handle("/health/", httpapi.Health(store, c.ReadyTimeout))
	}
	var handler http.Handler = operational
	if role == "worker" {
		log.Info("outbound admission configured", "rate_limit_enabled", c.RateLimitEnabled, "max_tenant_in_flight", c.MaxTenantInFlight)
	}
	if role == "api" {
		handler = httpapi.API(store, c.ReadyTimeout)
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		log.Error("listen failed")
		return 1
	}
	opsDone := make(chan error, 1)
	if role == "api" {
		opsListener, listenErr := net.Listen("tcp", c.APIOpsAddr)
		if listenErr != nil {
			listener.Close()
			log.Error("operational listen failed")
			return 1
		}
		opsServer := &http.Server{Handler: operational, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() {
			err := Serve(ctx, opsServer, opsListener, c.ShutdownTimeout)
			opsDone <- err
			if err != nil {
				stop()
			}
		}()
		log.Info("operational listener started", "address", opsListener.Addr().String())
	} else {
		opsDone <- nil
	}
	log.Info("started", "address", listener.Addr().String(), "profile", c.Profile)
	workerDone := make(chan struct{})
	if role == "worker" {
		id := security.ID()
		client := worker.NewClient(policy, c.RequestTimeout, c.WorkerConcurrency)
		defer client.CloseIdleConnections()
		runner := &worker.Runner{Telemetry: observation, Recovery: store, Repository: store, Sender: &worker.Sender{Client: client, Policy: policy, Vault: vault}, Log: log, ID: id, Concurrency: c.WorkerConcurrency, Poll: c.PollInterval, Lease: c.LeaseDuration, RequestTimeout: c.RequestTimeout, ShutdownTimeout: c.ShutdownTimeout}
		go func() { defer close(workerDone); runner.Run(ctx) }()
	} else {
		close(workerDone)
	}
	if e = Serve(ctx, server, listener, c.ShutdownTimeout); e != nil {
		log.Error("server failed", "error", e.Error())
		stop()
		<-workerDone
		<-opsDone
		return 1
	}
	<-workerDone
	if e = <-opsDone; e != nil {
		log.Error("operational server failed")
		return 1
	}
	log.Info("stopped")
	return 0
}

// Serve drains requests on cancellation, then forcibly closes if the grace period expires.
func Serve(ctx context.Context, s *http.Server, l net.Listener, grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	select {
	case e := <-done:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		e := s.Shutdown(shutdown)
		if e != nil {
			_ = s.Close()
		}
		serveErr := <-done
		if e != nil {
			return e
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}
