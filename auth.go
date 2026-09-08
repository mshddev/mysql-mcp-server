package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Every HTTP caller presents a bearer token, and every token belongs to a
// name: server.auth_tokens maps one to the other. The name is what the query
// log records as "caller", which is the whole reason tokens are per person
// rather than one for the team. A team that wants a single shared token
// defines one name and hands its token around; the log then names the team.

// bearerAuth guards next with the token set. A request whose token is not in
// the set gets 401 and never reaches next; one that matches carries its
// caller's name into the MCP handler, which the go-sdk hands to the tool as
// req.Extra.TokenInfo.UserID.
func bearerAuth(tokens map[string]string, next http.Handler) http.Handler {
	limiter := newWarnLimiter(warnInterval, warnMaxRemotes, time.Now)
	verifier := func(_ context.Context, presented string, r *http.Request) (*auth.TokenInfo, error) {
		name, ok := lookupToken(tokens, presented)
		if !ok {
			// A stale token from a real client is worth a line. The
			// address is the peer this server saw, which behind the
			// README's proxy is the proxy; the proxy's own access log has
			// the client, matched by time. Nothing from the token itself
			// is logged, a near-miss is still a secret. The line is
			// rate-limited per address: this is the one log write an
			// unauthenticated caller can cause, and unbounded it could
			// rotate the query lines the log exists for out of a
			// size-capped file.
			if repeats, ok := limiter.allow(remoteHost(r.RemoteAddr)); ok {
				slog.Warn("auth", "error", "unknown token", "remote", r.RemoteAddr, "repeats_suppressed", repeats)
			}
			return nil, fmt.Errorf("%w: unauthorized", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{UserID: name}, nil
	}
	// The tokens are static secrets with no expiry to check.
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
}

// warnInterval is how often one address can put an auth warning in the log;
// warnMaxRemotes bounds how many addresses the limiter remembers, so the
// limiter is not itself an unbounded growth an anonymous caller controls.
const (
	warnInterval   = time.Minute
	warnMaxRemotes = 1024
)

// remoteHost is the address without its port: one client presenting a stale
// token opens a new ephemeral port per request, and they are all the same
// caller. An address with no port is used as it is.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// warnLimiter admits one event per key per interval and counts the rest,
// so the next admitted line can say how many it stands for. It remembers at
// most max keys: when full, keys whose interval has passed are dropped, and
// if none has, a new key is refused rather than stored, which fails toward
// silence for the flood that would fill it and never toward growth.
type warnLimiter struct {
	interval time.Duration
	max      int
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]*warnEntry
}

type warnEntry struct {
	last       time.Time
	suppressed int
}

func newWarnLimiter(interval time.Duration, max int, now func() time.Time) *warnLimiter {
	return &warnLimiter{interval: interval, max: max, now: now, seen: make(map[string]*warnEntry)}
}

// allow reports whether key may log now, and how many events since its last
// admitted one were suppressed.
func (l *warnLimiter) allow(key string) (suppressed int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if e, seen := l.seen[key]; seen {
		if now.Sub(e.last) < l.interval {
			e.suppressed++
			return 0, false
		}
		suppressed, e.last, e.suppressed = e.suppressed, now, 0
		return suppressed, true
	}
	if len(l.seen) >= l.max {
		for k, e := range l.seen {
			if now.Sub(e.last) >= l.interval {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= l.max {
			return 0, false
		}
	}
	l.seen[key] = &warnEntry{last: now}
	return 0, true
}

// lookupToken finds the name whose token is presented. Every entry is
// compared, in constant time, whether or not an earlier one matched, so the
// time taken says nothing about which entry, if any, was the right one.
// Tokens are unique (LoadConfig checks), so at most one entry matches.
func lookupToken(tokens map[string]string, presented string) (string, bool) {
	var name string
	found := 0
	for n, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(presented)) == 1 {
			name = n
			found = 1
		}
	}
	return name, found == 1
}

// callerName is the caller the request authenticated as, or "" under stdio,
// where no token was checked: the caller is whoever launched the process.
func callerName(info *auth.TokenInfo) string {
	if info == nil {
		return ""
	}
	return info.UserID
}
