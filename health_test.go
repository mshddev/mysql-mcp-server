package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeClock and countingPing drive the cache without sleeping or a database.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

type countingPing struct {
	calls int
	err   error
}

func (p *countingPing) ping(context.Context) error {
	p.calls++
	return p.err
}

func newTestHealth(p *countingPing, clock *fakeClock) *health {
	h := newHealth(p.ping)
	h.now = clock.now
	return h
}

func get(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestRoutesProbesNeedNoToken(t *testing.T) {
	mcpCalled := false
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcpCalled = true })
	handler := routes(map[string]string{"probe": "secret"}, newTestHealth(&countingPing{}, &fakeClock{time.Now()}), mcpHandler)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := get(t, handler, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
			t.Errorf("GET %s: body %q", path, got)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type %q", path, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control %q", path, cc)
		}
	}
	if mcpCalled {
		t.Error("a probe reached the MCP handler")
	}
}

func TestRoutesProbesAreGetOnly(t *testing.T) {
	handler := routes(map[string]string{"probe": "secret"}, newTestHealth(&countingPing{}, &fakeClock{time.Now()}), http.NotFoundHandler())
	for _, path := range []string{"/healthz", "/readyz"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			if rec := get(t, handler, method, path); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405", method, path, rec.Code)
			}
		}
		// HEAD is what some checkers send; it must not need a token either.
		if rec := get(t, handler, http.MethodHead, path); rec.Code != http.StatusOK {
			t.Errorf("HEAD %s: status %d, want 200", path, rec.Code)
		}
	}
}

func TestRoutesEverythingElseIsGuarded(t *testing.T) {
	mcpCalled := false
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcpCalled = true })
	handler := routes(map[string]string{"probe": "secret"}, newTestHealth(&countingPing{}, &fakeClock{time.Now()}), mcpHandler)

	for _, path := range []string{"/", "/mcp", "/health", "/healthz/", "/readyz/x"} {
		rec := get(t, handler, http.MethodPost, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated POST %s: status %d, want 401", path, rec.Code)
		}
	}
	if mcpCalled {
		t.Fatal("an unauthenticated request reached the MCP handler")
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !mcpCalled {
		t.Error("an authenticated request did not reach the MCP handler")
	}
}

func TestReadyzReportsDegraded(t *testing.T) {
	p := &countingPing{err: errors.New("dial tcp: connection refused")}
	handler := routes(map[string]string{"probe": "secret"}, newTestHealth(p, &fakeClock{time.Now()}), http.NotFoundHandler())

	rec := get(t, handler, http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"degraded"}` {
		t.Errorf("body %q", got)
	}
	if strings.Contains(rec.Body.String(), "refused") {
		t.Error("the error detail leaked into the unauthenticated response")
	}
}

func TestReadyCachesWithinTTL(t *testing.T) {
	p := &countingPing{}
	clock := &fakeClock{time.Now()}
	h := newTestHealth(p, clock)

	for i := 0; i < 50; i++ {
		if err := h.ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("%d pings within one TTL, want 1", p.calls)
	}

	clock.t = clock.t.Add(readyTTL)
	h.ready(context.Background())
	if p.calls != 2 {
		t.Fatalf("%d pings after the TTL elapsed, want 2", p.calls)
	}
}

func TestReadyCachesFailuresToo(t *testing.T) {
	// A dead database must not turn every probe into a fresh dial attempt.
	p := &countingPing{err: errors.New("down")}
	clock := &fakeClock{time.Now()}
	h := newTestHealth(p, clock)

	for i := 0; i < 10; i++ {
		if err := h.ready(context.Background()); err == nil {
			t.Fatal("expected the cached failure")
		}
	}
	if p.calls != 1 {
		t.Fatalf("%d pings for a failing database within one TTL, want 1", p.calls)
	}

	p.err = nil
	clock.t = clock.t.Add(readyTTL)
	if err := h.ready(context.Background()); err != nil {
		t.Fatalf("recovery not observed after the TTL: %v", err)
	}
}

func TestReadyIgnoresTheCallersCancellation(t *testing.T) {
	// A prober that hangs up mid-ping must not poison the shared cache.
	p := &countingPing{}
	h := newTestHealth(p, &fakeClock{time.Now()})
	h.ping = func(ctx context.Context) error {
		p.calls++
		return ctx.Err()
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.ready(cancelled); err != nil {
		t.Fatalf("a cancelled caller failed the ping: %v", err)
	}
	if err := h.ready(context.Background()); err != nil {
		t.Fatalf("the cached result carried the caller's cancellation: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("%d pings, want 1", p.calls)
	}
}

func TestReadyBoundsThePing(t *testing.T) {
	h := newHealth(func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("ping context has no deadline")
		}
		if remaining := time.Until(dl); remaining > readyTimeout {
			t.Fatalf("deadline %v away, want at most %v", remaining, readyTimeout)
		}
		return nil
	})
	if err := h.ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Integration: the real pool answers a ping against a seeded database.
func TestPoolPing(t *testing.T) {
	p := newTestPool(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := p.ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// The connection went back to the pool rather than being leaked.
	if len(p.sem) != 0 {
		t.Fatalf("%d pool slots still held after ping", len(p.sem))
	}
}

func TestPoolPingUnreachable(t *testing.T) {
	cfg := testConfig(t)
	cfg.Database.Port = 1 // nothing listens here
	p := NewPool(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := p.ping(ctx); err == nil {
		t.Fatal("ping against a closed port succeeded")
	}
	if len(p.sem) != 0 {
		t.Fatalf("%d pool slots still held after a failed ping", len(p.sem))
	}
}
