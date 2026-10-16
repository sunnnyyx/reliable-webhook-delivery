package process

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestGracefulShutdownDrainsRequest(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("finished"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, s, l, time.Second) }()
	response := make(chan error, 1)
	go func() {
		r, e := http.Get("http://" + l.Addr().String())
		if e == nil {
			defer r.Body.Close()
			b, re := io.ReadAll(r.Body)
			e = re
			if string(b) != "finished" {
				e = io.ErrUnexpectedEOF
			}
		}
		response <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	close(release)
	select {
	case e := <-response:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request hung")
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung")
	}
}
func TestShutdownDeadlineClosesConnections(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release })}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, s, l, 20*time.Millisecond) }()
	response := make(chan error, 1)
	go func() {
		r, e := http.Get("http://" + l.Addr().String())
		if r != nil {
			r.Body.Close()
		}
		response <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case e := <-done:
		if e != context.DeadlineExceeded {
			t.Fatalf("expected grace deadline, got %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case e := <-response:
		if e == nil {
			t.Fatal("unfinished connection stayed open")
		}
	case <-time.After(time.Second):
		t.Fatal("client hung")
	}
}
