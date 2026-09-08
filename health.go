package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Health probes exist for machines: a load balancer deciding whether to send
// traffic here, a container runtime deciding whether to restart the process,
// an uptime monitor deciding whether to page someone. Those callers send a
// bare GET with no body and usually no headers, which is why these two paths
// sit in front of the bearer check. They give away nothing but up/down.

// readyTTL is how long one readiness result is reused. It bounds the database
// work an anonymous caller can cause: however many probes arrive, the
// database sees at most one ping per TTL.
const readyTTL = 5 * time.Second

// readyTimeout caps a single readiness ping. It also covers waiting for a
// free pool slot, so a pool saturated for this long reports not-ready, which
// is the truth: the server can't take a query right now either.
const readyTimeout = 3 * time.Second

// ping proves the database answers right now. acquire either pings an idle
// connection or dials a fresh one, and both are a real round-trip.
func (p *Pool) ping(ctx context.Context) error {
	conn, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	p.release(conn, false)
	return nil
}

// health serializes and caches readiness pings. The mutex is held across the
// ping on purpose: concurrent probes queue behind one database round-trip and
// all read its result, rather than each starting their own.
type health struct {
	ping func(context.Context) error
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	checked time.Time
	lastErr error
}

func newHealth(ping func(context.Context) error) *health {
	return &health{ping: ping, ttl: readyTTL, now: time.Now}
}

// ready answers from the cache when it can. The ping deliberately ignores the
// caller's context: its result is shared with every prober for the next TTL,
// so one prober hanging up mid-ping must not turn into five seconds of
// "degraded" for the rest. readyTimeout is the only bound.
func (h *health) ready(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.checked.IsZero() && h.now().Sub(h.checked) < h.ttl {
		return h.lastErr
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout)
	defer cancel()
	h.lastErr = h.ping(ctx)
	h.checked = h.now()
	return h.lastErr
}

// routes assembles the server's handler: two unauthenticated probes, and the
// bearer-guarded MCP handler on every other path. tokens is the caller set
// from server.auth_tokens.
func routes(tokens map[string]string, h *health, mcpHandler http.Handler) http.Handler {
	// The probes are registered without a method: with a "/" catch-all in the
	// mux, a "GET /healthz" pattern would send a POST to the bearer check and
	// answer 401 instead of 405. probe checks the method itself.
	mux := http.NewServeMux()
	mux.Handle("/healthz", probe(func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	}))
	mux.Handle("/readyz", probe(func(w http.ResponseWriter, r *http.Request) {
		if err := h.ready(r.Context()); err != nil {
			// The detail goes to the log, where it is useful; the anonymous
			// caller only learns that the database is not answering.
			slog.Warn("readiness", "error", err.Error())
			writeStatus(w, http.StatusServiceUnavailable, "degraded")
			return
		}
		writeStatus(w, http.StatusOK, "ok")
	}))
	mux.Handle("/", bearerAuth(tokens, mcpHandler))
	return mux
}

// probe admits GET and HEAD, which is all a health checker sends.
func probe(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	})
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write([]byte(`{"status":"` + status + `"}` + "\n"))
}
