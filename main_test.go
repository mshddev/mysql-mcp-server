package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerAuth(t *testing.T) {
	const token = "s3cret-token"

	tests := []struct {
		name       string
		header     string
		setHeader  bool
		wantStatus int
	}{
		{
			name:       "correct token",
			header:     "Bearer " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			// RFC 7235: the scheme name is case-insensitive.
			name:       "lowercase scheme",
			header:     "bearer " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "mixed case scheme",
			header:     "BeArEr " + token,
			setHeader:  true,
			wantStatus: http.StatusOK,
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
			name:       "token has trailing whitespace",
			header:     "Bearer " + token + " ",
			setHeader:  true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "extra space before the token",
			header:     "Bearer  " + token,
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
			// Regression: slicing a header shorter than "Bearer " must not panic.
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			if tt.setHeader {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			bearerAuth(token, next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			wantCalled := tt.wantStatus == http.StatusOK
			if called != wantCalled {
				t.Errorf("next handler called = %v, want %v", called, wantCalled)
			}
			if !wantCalled && !strings.Contains(rec.Body.String(), "unauthorized") {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), "unauthorized")
			}
		})
	}
}

// The middleware must pass the request through untouched, including the body
// the MCP handler needs to read.
func TestBearerAuthForwardsRequest(t *testing.T) {
	const token = "s3cret-token"
	var gotMethod, gotPath, gotBody string

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	bearerAuth(token, next).ServeHTTP(httptest.NewRecorder(), req)

	if gotMethod != http.MethodPost || gotPath != "/mcp" || gotBody != `{"jsonrpc":"2.0"}` {
		t.Errorf("forwarded %s %s body %q, want POST /mcp body %q",
			gotMethod, gotPath, gotBody, `{"jsonrpc":"2.0"}`)
	}
}
