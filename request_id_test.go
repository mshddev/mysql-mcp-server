package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestValidRequestID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"abc-123.x_Y", true},
		{"a", true},
		{strings.Repeat("x", 64), true},
		{"", false},
		{strings.Repeat("x", 65), false},
		{"a b", false},
		{" a", false},
		{"a\n", false},
		{"a\t", false},
		{"é", false},
		{"<x>", false},
		{"a/b", false},
	} {
		if got := validRequestID(tc.id); got != tc.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// hexID is the shape of a generated id: 8 random bytes in lowercase hex.
var hexID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestNewRequestID(t *testing.T) {
	a, b := newRequestID(), newRequestID()
	for _, id := range []string{a, b} {
		if !hexID.MatchString(id) {
			t.Errorf("newRequestID() = %q, want 16 lowercase hex characters", id)
		}
	}
	if a == b {
		t.Errorf("two generated ids are both %q", a)
	}
}

// A header that passes is used verbatim; no header at all, as under stdio,
// or one that fails gets a generated id with nothing of the rejected value
// in it.
func TestRequestIDFromHeader(t *testing.T) {
	if got := requestID(nil); !hexID.MatchString(got) {
		t.Errorf("requestID(nil) = %q, want a generated id", got)
	}
	h := http.Header{}
	h.Set("X-Request-Id", "req-42")
	if got := requestID(h); got != "req-42" {
		t.Errorf("requestID = %q, want the supplied req-42", got)
	}
	h.Set("X-Request-Id", "req 42")
	if got := requestID(h); !hexID.MatchString(got) {
		t.Errorf("requestID = %q for a rejected header, want a generated id", got)
	}
}

// The id on the logged query line is the caller's X-Request-Id when it is
// acceptable and a generated one otherwise, and it is the first attribute so
// it reads first. With nothing listening on the database port every call
// fails, which exercises the error line; the integration test below covers
// the success line.
func TestQueryLogRequestID(t *testing.T) {
	cfg := unitConfig()
	// A port nothing listens on: the dial is refused at once and the query
	// takes the error path.
	ln := listen(t)
	cfg.Database.Host, cfg.Database.Port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	for _, tc := range []struct{ name, header, want string }{
		{"supplied", "req-42", "req-42"},
		{"absent", "", ""},
		{"whitespace", "req 42", ""},
		{"too long", strings.Repeat("x", 65), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, line := queryLogLine(t, cfg, NewPool(cfg), tc.header)
			if rec["level"] != "ERROR" {
				t.Fatalf("query line is not the error line: %s", line)
			}
			checkRequestID(t, rec, line, tc.want)
		})
	}
}

// Integration: the success line carries the id too.
func TestQueryLogRequestIDOnSuccess(t *testing.T) {
	pool := newTestPool(t, nil)
	rec, line := queryLogLine(t, pool.cfg, pool, "req-42")
	if rec["level"] != "INFO" {
		t.Fatalf("query line is not the success line: %s", line)
	}
	checkRequestID(t, rec, line, "req-42")
}

// checkRequestID asserts the line's request_id is want, or a generated id
// when want is empty, and that it precedes the query in the raw line.
func checkRequestID(t *testing.T, rec map[string]any, line, want string) {
	t.Helper()
	got, _ := rec["request_id"].(string)
	if want != "" && got != want {
		t.Errorf("request_id = %q, want %q", got, want)
	}
	if want == "" && !hexID.MatchString(got) {
		t.Errorf("request_id = %q, want a generated 16-hex id", got)
	}
	// "msg":"query" is not "query":, so the index finds the attribute.
	if i, j := strings.Index(line, `"request_id":`), strings.Index(line, `"query":`); i < 0 || j < 0 || i > j {
		t.Errorf("request_id does not precede the query in %s", line)
	}
}

// queryLogLine drives one tools/call through the handler chain main serves,
// on a real listener, sending X-Request-Id: header when header is not empty,
// and returns the "query" line the server logged, decoded and raw.
func queryLogLine(t *testing.T, cfg *Config, pool *Pool, header string) (map[string]any, string) {
	t.Helper()
	var logs bytes.Buffer
	server := newMCPServer(cfg, pool, slog.New(slog.NewJSONHandler(&logs, nil)))
	srv := httptest.NewServer(routes(cfg.Server.AuthToken, newHealth(pool.ping),
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: true})))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL,
		HTTPClient:           &http.Client{Transport: headerTransport{token: cfg.Server.AuthToken, requestID: header}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	// The result is not the subject: without a database the call fails, and
	// the error line must carry the id just as the success line does.
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"sql": "SELECT 1 AS one"},
	})
	session.Close()
	// Close waits for the handlers to return, so the buffer is quiet before
	// it is read.
	srv.Close()

	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec["msg"] == "query" {
			return rec, line
		}
	}
	t.Fatalf("no query line in the log:\n%s", logs.String())
	return nil, ""
}

// headerTransport adds the bearer token and, when set, an X-Request-Id to
// every request the MCP client sends.
type headerTransport struct{ token, requestID string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	if h.requestID != "" {
		r.Header.Set("X-Request-Id", h.requestID)
	}
	return http.DefaultTransport.RoundTrip(r)
}
