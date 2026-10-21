// Web supervises the existing processes behind a single platform web port.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"relayforge/internal/config"
	"relayforge/internal/migrate"
	"strconv"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if os.Getenv("PROFILE") != "production" {
		log.Error("web requires PROFILE=production")
		return 1
	}
	c, err := config.Load(os.Getenv)
	if err != nil {
		log.Error("invalid configuration")
		return 1
	}
	origin, err := url.Parse(os.Getenv("PUBLIC_URL"))
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		log.Error("PUBLIC_URL must be an HTTPS origin")
		return 1
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 1
	}
	if os.Getenv("SUPABASE_URL") == "" || os.Getenv("SUPABASE_PUBLISHABLE_KEY") == "" || len(c.EncryptionKey) != 32 {
		log.Error("identity and encryption settings are required")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	migrationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = migrate.Run(migrationCtx, c.DatabaseURL, "up")
	cancel()
	if err != nil {
		log.Error("migration failed")
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		return 1
	}
	dir := filepath.Dir(executable)
	exited := make(chan error, 3)
	children := []*exec.Cmd{}
	env := append(os.Environ(), "API_ADDR=127.0.0.1:8080", "API_OPS_ADDR=127.0.0.1:9091", "WORKER_ADDR=127.0.0.1:8081", "CONSOLE_ADDR=127.0.0.1:8091", "CONSOLE_API_URL=http://127.0.0.1:8080")
	defer func() {
		for _, child := range children {
			_ = child.Process.Signal(syscall.SIGTERM)
		}
		timer := time.NewTimer(18 * time.Second)
		defer timer.Stop()
		for range children {
			select {
			case <-exited:
			case <-timer.C:
				for _, child := range children {
					_ = child.Process.Kill()
				}
				return
			}
		}
	}()
	for _, name := range []string{"api", "worker", "console"} {
		child := exec.Command(filepath.Join(dir, name))
		child.Env = env
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err = child.Start(); err != nil {
			log.Error("child start failed", "process", name)
			return 1
		}
		children = append(children, child)
		go func() { exited <- child.Wait() }()
	}
	apiURL, _ := url.Parse("http://127.0.0.1:8080")
	consoleURL, _ := url.Parse("http://127.0.0.1:8091")
	handler := newGateway(origin.Host, apiURL, consoleURL, "http://127.0.0.1:8081/health/ready")
	server := &http.Server{Addr: net.JoinHostPort("", port), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	serving := make(chan error, 1)
	go func() { serving <- server.ListenAndServe() }()
	log.Info("web gateway started", "port", port)
	result := 0
	select {
	case <-ctx.Done():
	case <-exited:
		result = 1
		log.Error("child exited; shutting down")

		exited <- errors.New("already exited")
	case e := <-serving:
		if !errors.Is(e, http.ErrServerClosed) {
			result = 1
			log.Error("gateway stopped")
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return result
}

func newGateway(host string, apiURL, consoleURL *url.URL, workerHealth string) http.Handler {
	api := httputil.NewSingleHostReverseProxy(apiURL)
	ui := httputil.NewSingleHostReverseProxy(consoleURL)
	proxyError := func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "Service starting. Retry shortly.", 503)
	}
	api.ErrorHandler = proxyError
	ui.ErrorHandler = proxyError
	healthClient := &http.Client{Timeout: 2 * time.Second}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Platform probes do not always send the public Host. This route contains no tenant data.
		if r.URL.Path == "/health/ready" && r.Method == "GET" {
			for _, address := range []string{apiURL.String() + "/health/ready", workerHealth, consoleURL.String() + "/auth/config"} {
				req, _ := http.NewRequestWithContext(r.Context(), "GET", address, nil)
				req.Host = host
				resp, e := healthClient.Do(req)
				if e != nil {
					http.Error(w, "not ready", 503)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					http.Error(w, "not ready", 503)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		if r.Host != host {
			http.Error(w, "Invalid host", 403)
			return
		}
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/v1/" {
			api.ServeHTTP(w, r)
		} else {
			ui.ServeHTTP(w, r)
		}
	})
	return handler
}
