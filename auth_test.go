package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// twoCallers is the token set the auth tests share: two names, two tokens.
var twoCallers = map[string]string{
	"alice": "alice-s3cret-token",
	"bob":   "bob-s3cret-token",
}

func TestBearerAuth(t *testing.T) {
	token := twoCallers["alice"]

	tests := []struct {
		name       string
		header     string
		setHeader  bool
		wantStatus int
		wantCaller string
	}{
		{
			name:       "correct token",
			header:     "Bearer " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "alice",
		},
		{
			name:       "the other caller's token",
			header:     "Bearer " + twoCallers["bob"],
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "bob",
		},
		{
			// RFC 7235: the scheme name is case-insensitive.
			name:       "lowercase scheme",
			header:     "bearer " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "alice",
		},
		{
			name:       "mixed case scheme",
			header:     "BeArEr " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "alice",
		},
		{
			// The header is split on whitespace, so surrounding space is
			// not part of the token. Tokens themselves can't contain any
			// (LoadConfig refuses them), so nothing is lost to the trim.
			name:       "token has trailing whitespace",
			header:     "Bearer " + token + " ",
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "alice",
		},
		{
			name:       "extra space before the token",
			header:     "Bearer  " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
			wantCaller: "alice",
		},
		{
			name:       "wrong token",
			header:     "Bearer wrong-token",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token is a prefix of the real one",
			header:     "Bearer " + token[:4],
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token with the real one as a prefix",
			header:     "Bearer " + token + "x",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// A name is not a token, even though both are in the config.
			name:       "caller name as the token",
			header:     "Bearer alice",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no authorization header",
			setHeader:  false,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "empty header",
			header:     "",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "header shorter than the scheme",
			header:     "Bear",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "scheme with no trailing space",
			header:     "Bearer",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "scheme only",
			header:     "Bearer ",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "single character header",
			header:     "B",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "different scheme",
			header:     "Basic " + token,
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token without a scheme",
			header:     token,
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "two tokens",
			header:     "Bearer " + token + " " + twoCallers["bob"],
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var gotCaller string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				gotCaller = callerName(auth.TokenInfoFromContext(r.Context()))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			if tt.setHeader {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			bearerAuth(twoCallers, next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			wantCalled := tt.wantStatus == http.StatusOK
			if called != wantCalled {
				t.Errorf("next handler called = %v, want %v", called, wantCalled)
			}
			if gotCaller != tt.wantCaller {
				t.Errorf("caller = %q, want %q", gotCaller, tt.wantCaller)
			}
			if !wantCalled {
				// A refusal says nothing about the set: not which names
				// exist, not how close the token came.
				for _, secret := range []string{"alice", "bob", token} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Errorf("401 body %q leaks %q", rec.Body.String(), secret)
					}
				}
			}
		})
	}
}

// The middleware must pass the request through untouched, including the body
// the MCP handler needs to read.
func TestBearerAuthForwardsRequest(t *testing.T) {
	var gotMethod, gotPath, gotBody string

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer "+twoCallers["bob"])
	bearerAuth(twoCallers, next).ServeHTTP(httptest.NewRecorder(), req)

	if gotMethod != http.MethodPost || gotPath != "/mcp" || gotBody != `{"jsonrpc":"2.0"}` {
		t.Errorf("forwarded %s %s body %q, want POST /mcp body %q",
			gotMethod, gotPath, gotBody, `{"jsonrpc":"2.0"}`)
	}
}

func TestLookupToken(t *testing.T) {
	for _, tc := range []struct {
		presented string
		wantName  string
		wantOK    bool
	}{
		{twoCallers["alice"], "alice", true},
		{twoCallers["bob"], "bob", true},
		{"", "", false},
		{"alice", "", false},
		{twoCallers["alice"][:5], "", false},
	} {
		name, ok := lookupToken(twoCallers, tc.presented)
		if name != tc.wantName || ok != tc.wantOK {
			t.Errorf("lookupToken(%q) = %q, %v; want %q, %v", tc.presented, name, ok, tc.wantName, tc.wantOK)
		}
	}
	if name, ok := lookupToken(nil, "anything"); ok || name != "" {
		t.Errorf("lookupToken on an empty set = %q, %v; want no match", name, ok)
	}
}

func TestCallerName(t *testing.T) {
	if got := callerName(nil); got != "" {
		t.Errorf("callerName(nil) = %q, want empty (stdio has no caller)", got)
	}
	if got := callerName(&auth.TokenInfo{UserID: "alice"}); got != "alice" {
		t.Errorf("callerName = %q, want alice", got)
	}
}

func TestWarnLimiter(t *testing.T) {
	clock := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	l := newWarnLimiter(time.Minute, 2, func() time.Time { return clock })

	if n, ok := l.allow("a"); !ok || n != 0 {
		t.Fatalf("first event for a: (%d, %v), want admitted with nothing suppressed", n, ok)
	}
	for i := 0; i < 3; i++ {
		if _, ok := l.allow("a"); ok {
			t.Fatalf("repeat %d for a within the interval was admitted", i)
		}
	}
	if n, ok := l.allow("b"); !ok || n != 0 {
		t.Fatalf("first event for b: (%d, %v), want admitted", n, ok)
	}
	// The table is full of fresh keys: a third address is refused, not stored.
	if _, ok := l.allow("c"); ok {
		t.Fatal("c was admitted while the table was full of fresh keys")
	}
	if len(l.seen) != 2 {
		t.Fatalf("table holds %d keys, want 2", len(l.seen))
	}

	clock = clock.Add(time.Minute)
	// a's interval has passed: admitted again, carrying the count it stood for.
	if n, ok := l.allow("a"); !ok || n != 3 {
		t.Fatalf("a after the interval: (%d, %v), want admitted with 3 suppressed", n, ok)
	}
	// b is stale too, so c now finds room by evicting it.
	if _, ok := l.allow("c"); !ok {
		t.Fatal("c was refused after the stale keys could be evicted")
	}
	if _, stale := l.seen["b"]; stale {
		t.Error("b survived the eviction sweep")
	}
}

func TestRemoteHost(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:54321": "127.0.0.1",
		"[::1]:8080":      "::1",
		"192.0.2.1":       "192.0.2.1",
		"":                "",
	} {
		if got := remoteHost(addr); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A stream of bad tokens from one address writes one warning, not one per
// request, and the line names how many it stands for once the interval
// passes. Addresses are told apart by host, not port.
func TestBearerAuthWarnsOncePerAddress(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	h := bearerAuth(twoCallers, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	send := func(remote string) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer stale-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}
	for port := 1; port <= 5; port++ {
		send(fmt.Sprintf("10.0.0.1:%d", 40000+port))
	}
	send("10.0.0.2:40001")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d auth lines, want one per address:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], `"remote":"10.0.0.1:40001"`) || !strings.Contains(lines[1], `"remote":"10.0.0.2:40001"`) {
		t.Errorf("lines name the wrong remotes:\n%s", logs.String())
	}
	for _, line := range lines {
		if strings.Contains(line, "stale-token") {
			t.Errorf("the token leaked into the log: %s", line)
		}
	}
}
