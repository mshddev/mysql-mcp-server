package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// unitConfig is a Config for tests that never reach a database: the pool it
// builds has nothing to dial and nothing to close.
func unitConfig() *Config {
	cfg := &Config{}
	cfg.Mode = modeReadOnly
	cfg.Server.AuthToken = "test-token"
	cfg.Limits.TimeoutSeconds = 10
	cfg.Limits.MaxResponseBytes = 8 << 20
	cfg.Limits.MaxConnections = 4
	return cfg
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// A stop request must let a request already being served finish, and the
// serve function must then return nil — the process exits 0 — inside the
// drain bound.
func TestServeHTTPDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		w.Write([]byte("done"))
	})}
	ln := listen(t)

	ctx, cancel := context.WithCancelCause(context.Background())
	const drain = 5 * time.Second
	returned := make(chan error, 1)
	go func() {
		returned <- serveHTTP(ctx, srv, ln, NewPool(unitConfig()), slog.New(slog.DiscardHandler), drain)
	}()

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/", ln.Addr()))
		if err != nil {
			got <- -1
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	<-started

	cancel(errors.New("test asked for a stop"))
	// The listener closes at once; the in-flight request must not.
	select {
	case err := <-returned:
		t.Fatalf("serveHTTP returned %v while a request was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Error("the listener still accepts connections after the stop request")
	}

	close(finish)
	if code := <-got; code != http.StatusOK {
		t.Errorf("in-flight request finished with status %d, want 200", code)
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("serveHTTP returned %v after a clean stop, want nil", err)
		}
	case <-time.After(drain):
		t.Fatal("serveHTTP did not return within the drain bound")
	}
}

// A request that outlives the drain is abandoned rather than holding the
// process open: serveHTTP still returns nil, and promptly.
func TestServeHTTPGivesUpAfterDrain(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
	})}
	ln := listen(t)

	ctx, cancel := context.WithCancelCause(context.Background())
	const drain = 300 * time.Millisecond
	returned := make(chan error, 1)
	go func() {
		returned <- serveHTTP(ctx, srv, ln, NewPool(unitConfig()), slog.New(slog.DiscardHandler), drain)
	}()
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/", ln.Addr()))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started

	stop := time.Now()
	cancel(errors.New("test asked for a stop"))
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("serveHTTP returned %v, want nil", err)
		}
		if took := time.Since(stop); took > drain+2*time.Second {
			t.Errorf("serveHTTP took %v to give up on a stuck request, drain is %v", took, drain)
		}
	case <-time.After(drain + 5*time.Second):
		t.Fatal("serveHTTP never gave up on a request that outlived the drain")
	}
}

// Serve failing is the exit-1 path and must surface as an error, not be
// mistaken for a stop request.
func TestServeHTTPReportsServeErrors(t *testing.T) {
	ln := listen(t)
	ln.Close() // Serve fails on the first Accept
	err := serveHTTP(context.Background(), &http.Server{Handler: http.NotFoundHandler()}, ln,
		NewPool(unitConfig()), slog.New(slog.DiscardHandler), time.Second)
	if err == nil {
		t.Fatal("serveHTTP returned nil for a listener that cannot accept")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serveHTTP reported %v, which is the clean-stop error, for a broken listener", err)
	}
}

// The drain follows the query timeout, so every running query can reach its
// own deadline and be killed, and stops short of systemd's 90s kill.
func TestDrainTimeout(t *testing.T) {
	cfg := unitConfig()
	for _, tc := range []struct {
		timeout int
		want    time.Duration
	}{
		{10, 15 * time.Second},
		{30, 35 * time.Second},
		{75, 80 * time.Second},
		{120, maxDrain},
	} {
		cfg.Limits.TimeoutSeconds = tc.timeout
		if got := drainTimeout(cfg); got != tc.want {
			t.Errorf("timeout_seconds=%d: drain %v, want %v", tc.timeout, got, tc.want)
		}
	}
	if maxDrain >= 90*time.Second {
		t.Errorf("maxDrain %v is not under systemd's default TimeoutStopSec of 90s", maxDrain)
	}
}

// Integration: Close hangs up whatever the pool kept idle.
func TestPoolCloseDrainsIdle(t *testing.T) {
	p := newTestPool(t, nil)
	mustQuery(t, p, "SELECT 1")
	if len(p.idle) != 1 {
		t.Fatalf("%d idle connections after a query, want 1", len(p.idle))
	}
	p.Close()
	if len(p.idle) != 0 {
		t.Fatalf("%d idle connections after Close, want 0", len(p.idle))
	}
	// The pool is still usable — a request that outlived the drain may
	// release into it — and dials afresh.
	mustQuery(t, p, "SELECT 1")
}
