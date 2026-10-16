//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"relayforge/internal/delivery"
	"relayforge/internal/endpoints"
	"relayforge/internal/netpolicy"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"relayforge/internal/worker"
	"sync"
	"testing"
	"time"
)

func TestRecoveryConcurrentAndFinite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Recovery")
	targets(t, s, c.TenantID, "order.created")
	var jobs []*delivery.Job
	for i := 0; i < 8; i++ {
		if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
			t.Fatal(e)
		}
		j, e := s.Claim(ctx, security.ID(), time.Minute)
		if e != nil || j == nil {
			t.Fatal(e)
		}
		jobs = append(jobs, j)
	}
	// Database-clock manipulation is test-only; no production failpoint switches.
	if _, e := s.DB.Exec(ctx, `UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Recover(ctx, 100); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var n int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE status='retry_scheduled' AND lease_token IS NULL AND cycle_attempt_count=1 AND lifetime_attempt_count=1`).Scan(&n); e != nil || n != 8 {
		t.Fatalf("recovered=%d err=%v", n, e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE outcome='lease_expired' AND finished_at IS NOT NULL`).Scan(&n); e != nil || n != 8 {
		t.Fatalf("attempts=%d err=%v", n, e)
	}
	for _, j := range jobs {
		ok, e := s.Complete(ctx, *j, delivery.Classify(200))
		if e != nil || ok {
			t.Fatal("stale completion accepted", e)
		}
	}
	// Disabled endpoints and future-due rows still age out, without new attempts.
	if _, e := s.DB.Exec(ctx, `UPDATE endpoints SET enabled=false; UPDATE deliveries SET cycle_started_at=clock_timestamp()-interval '25 hours',next_attempt_at=clock_timestamp()+interval '1 day'`); e != nil {
		t.Fatal(e)
	}
	if n, e := s.Recover(ctx, 3); e != nil || n != 3 {
		t.Fatalf("bounded recovery %d %v", n, e)
	}
	if _, e := s.Recover(ctx, 100); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE status='dead_letter' AND last_error_code='cycle_expired'`).Scan(&n); e != nil || n != 8 {
		t.Fatalf("aged=%d %v", n, e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts`).Scan(&n); e != nil || n != 8 {
		t.Fatalf("unexpected attempts %d %v", n, e)
	}
}

type crashSender struct{ ack bool }

func (s crashSender) Send(ctx context.Context, j delivery.Job) delivery.Result {
	if s.ack && os.Getenv("RF_CRASH_USE_SENDER") == "1" {
		policy, e := netpolicy.New("development", os.Getenv("RF_CRASH_DESTINATION"), nil, nil)
		if e != nil {
			panic(e)
		}
		vault, e := security.NewVault(bytes.Repeat([]byte{23}, 32))
		if e != nil {
			panic(e)
		}
		client := worker.NewClient(policy, time.Second, 1)
		sender := worker.Sender{Client: client, Policy: policy, Vault: vault}
		result := sender.Send(ctx, j)
		client.CloseIdleConnections()
		if result.HTTPStatus != 204 {
			panic("receiver rejected signed request")
		}
	} else if s.ack {
		req, e := http.NewRequestWithContext(ctx, "POST", os.Getenv("RF_CRASH_RECEIVER"), bytes.NewReader(j.Body))
		if e != nil {
			panic(e)
		}
		req.Header.Set("webhook-id", j.ID)
		response, e := http.DefaultClient.Do(req)
		if e != nil {
			panic(e)
		}
		response.Body.Close()
		if response.StatusCode != 204 {
			panic("receiver rejected")
		}
	}
	os.Stdout.WriteString("CRASH_GATE\n")
	// Parent kills the process only after this gate, before Complete can run.
	select {}
}
func TestCrashWorkerHelper(t *testing.T) {
	if os.Getenv("RF_CRASH_HELPER") != "1" {
		return
	}
	cfg, e := pgxpool.ParseConfig(os.Getenv("RF_CRASH_DB"))
	if e != nil {
		t.Fatal(e)
	}
	pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := &postgres.Store{DB: pool}
	r := worker.Runner{Repository: s, Sender: crashSender{ack: os.Getenv("RF_CRASH_ACK") == "1"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ID: security.ID(), Concurrency: 1, Poll: time.Millisecond, Lease: 300 * time.Millisecond, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
	r.Run(context.Background())
}
func TestKilledWorkerAndAmbiguousAcknowledgment(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_claim", true: "after_remote_effect"}[ack], func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			c := credentials(t, s, "Crash")
			targets(t, s, c.TenantID, "order.created")
			if _, e := s.DB.Exec(ctx, `CREATE TABLE crash_effects(delivery_id uuid,body bytea)`); e != nil {
				t.Fatal(e)
			}
			if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
				t.Fatal(e)
			}
			cfg := s.DB.Config().ConnConfig
			// Explicitly propagate runtime schema settings to the subprocess.
			u, e := url.Parse(os.Getenv("INTEGRATION_DATABASE_URL"))
			if e != nil {
				t.Fatal(e)
			}
			q := u.Query()
			q.Set("search_path", cfg.RuntimeParams["search_path"])
			u.RawQuery = q.Encode()
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, e := io.ReadAll(r.Body)
				if e == nil {
					_, e = s.DB.Exec(r.Context(), `INSERT INTO crash_effects VALUES($1,$2)`, r.Header.Get("webhook-id"), body)
				}
				if e != nil {
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(204)
			}))
			defer receiver.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWorkerHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "RF_CRASH_HELPER=1", "RF_CRASH_DB="+u.String(), "RF_CRASH_RECEIVER="+receiver.URL)
			if ack {
				cmd.Env = append(cmd.Env, "RF_CRASH_ACK=1")
			}
			stdout, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			gate := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if scanner.Text() == "CRASH_GATE" {
						gate <- true
						return
					}
				}
				gate <- false
			}()
			select {
			case ok := <-gate:
				if !ok {
					t.Fatal("crash gate missing")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("crash gate timeout")
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = cmd.Wait()
			// Observe real expiry and jittered due time; never force either timestamp.
			deadline := time.Now().Add(6 * time.Second)
			var second *delivery.Job
			for time.Now().Before(deadline) {
				if _, e = s.Recover(ctx, 100); e != nil {
					t.Fatal(e)
				}
				second, e = s.Claim(ctx, security.ID(), time.Minute)
				if e != nil {
					t.Fatal(e)
				}
				if second != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if second == nil || second.Attempt != 2 {
				t.Fatal("crashed claim was not recovered")
			}
			if _, e = s.DB.Exec(ctx, `INSERT INTO crash_effects VALUES($1,$2)`, second.ID, second.Body); e != nil {
				t.Fatal(e)
			}
			if ok, e := s.Complete(ctx, *second, delivery.Classify(204)); e != nil || !ok {
				t.Fatal(e)
			}
			rows, e := s.DB.Query(ctx, `SELECT delivery_id::text,body FROM crash_effects`)
			if e != nil {
				t.Fatal(e)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var id string
				var b []byte
				if e = rows.Scan(&id, &b); e != nil {
					t.Fatal(e)
				}
				if id != second.ID || !bytes.Equal(b, second.Body) {
					t.Fatal("recovery changed identity or bytes")
				}
				n++
			}
			if e = rows.Err(); e != nil {
				t.Fatal(e)
			}
			want := 1
			if ack {
				want = 2
			}
			if n != want {
				t.Fatalf("remote effects=%d want=%d", n, want)
			}
		})
	}
}

func TestRecoveryBudgetAndAtomicRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Budget")
	targets(t, s, c.TenantID, "order.created")
	if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx, security.ID(), time.Minute)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if n, e := s.Recover(ctx, 100); e != nil || n != 0 {
		t.Fatal("unexpired lease recovered", n, e)
	}
	// Corrupt the attempt inside this isolated test to prove recovery is atomic.
	if _, e = s.DB.Exec(ctx, `UPDATE deliveries SET lease_expires_at=clock_timestamp()-interval '1 second'; UPDATE delivery_attempts SET outcome='success',finished_at=clock_timestamp()`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Recover(ctx, 100); e == nil {
		t.Fatal("expected invariant failure")
	}
	var status string
	if e = s.DB.QueryRow(ctx, `SELECT status FROM deliveries WHERE id=$1`, j.ID).Scan(&status); e != nil || status != "in_flight" {
		t.Fatal("partial recovery", status, e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE delivery_attempts SET outcome='started',finished_at=NULL; UPDATE deliveries SET cycle_attempt_count=10,lifetime_attempt_count=10`); e != nil {
		t.Fatal(e)
	}
	if n, e := s.Recover(ctx, 100); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var code string
	if e = s.DB.QueryRow(ctx, `SELECT status,last_error_code FROM deliveries WHERE id=$1`, j.ID).Scan(&status, &code); e != nil || status != "dead_letter" || code != "attempt_budget_exhausted" {
		t.Fatal(status, code, e)
	}
	if n, e := s.Recover(ctx, 100); e != nil || n != 0 {
		t.Fatal("repeated recovery", n, e)
	}
}

// Each restart uses a new runner and Store while retaining committed PostgreSQL state.
func TestReceiverOutageRestartAndRetryAfter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := credentials(t, s, "Outage")
	var mu sync.Mutex
	var bodies [][]byte
	var at []time.Time
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		at = append(at, time.Now())
		n := len(bodies)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(503)
		} else if n == 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer receiver.Close()
	// The production sender permits only its exact loopback development exception.
	u, _ := url.Parse(receiver.URL)
	destination := "http://localhost:" + u.Port() + "/webhooks"
	policy, e := netpolicy.New("development", destination, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	s.Policy = policy
	if _, _, e := s.CreateEndpoint(ctx, c.TenantID, endpoints.Create{URL: destination, Subscriptions: []string{"order.created"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Accept(ctx, c.TenantID, security.ID(), []byte(orderBody)); e != nil {
		t.Fatal(e)
	}
	client := worker.NewClient(policy, time.Second, 1)
	defer client.CloseIdleConnections()
	sender := &worker.Sender{Client: client, Policy: policy, Vault: s.Vault}
	for attempt := 1; attempt <= 4; attempt++ {
		fresh := &postgres.Store{DB: s.DB, Vault: s.Vault, Policy: policy}
		observed := &observedCompletion{Store: fresh, done: make(chan delivery.Job, 1), release: make(chan struct{})}
		runctx, cancel := context.WithCancel(ctx)
		runner := worker.Runner{Repository: observed, Recovery: fresh, Sender: sender, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ID: security.ID(), Concurrency: 1, Poll: 10 * time.Millisecond, Lease: time.Minute, RequestTimeout: time.Second, ShutdownTimeout: time.Second}
		stopped := make(chan struct{})
		go func() { runner.Run(runctx); close(stopped) }()
		select {
		case job := <-observed.done:
			if job.Attempt != attempt {
				t.Error("wrong retry attempt", job.Attempt)
			}
		case <-time.After(12 * time.Second):
			close(observed.release)
			cancel()
			<-stopped
			t.Fatal("durable retry missing")
		}
		cancel()
		close(observed.release)
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("restart shutdown blocked")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 4 {
		t.Fatal(len(bodies))
	}
	for _, b := range bodies {
		if !bytes.Equal(b, bodies[0]) {
			t.Fatal("retry bytes changed")
		}
	}
	if at[3].Sub(at[2]) < time.Second {
		t.Fatal("Retry-After ignored")
	}
	var status string
	if e = s.DB.QueryRow(ctx, `SELECT status FROM deliveries`).Scan(&status); e != nil || status != "delivered" {
		t.Fatal(status, e)
	}
}

type observedCompletion struct {
	*postgres.Store
	done    chan delivery.Job
	release chan struct{}
}

func (s *observedCompletion) Complete(ctx context.Context, j delivery.Job, r delivery.Result) (bool, error) {
	ok, e := s.Store.Complete(ctx, j, r)
	if ok && e == nil {
		s.done <- j
		<-s.release
	}
	return ok, e
}
