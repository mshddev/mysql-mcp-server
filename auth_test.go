package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
