package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The query log line is the one place a WHERE literal, a write's VALUES, or
// a database error that echoes the statement can put a row value on disk.
// With masking.values on, the same detectors that scrub a result cell run
// over the logged copy of the SQL and of the error text; with it off the
// line is unchanged. Only the logged copy: the statement the database runs
// is the original, and the integration test below proves that by the rows
// it gets back.

const (
	scrubEmail = "andi@example.com"
	scrubPhone = "081234567890"
)

func valuesMasker(t *testing.T, entries []string) *Masker {
	t.Helper()
	m, err := NewMasker(&MaskingConfig{Values: entries})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// queryLogRecord drives one tools/call through the server in-process, the
// way TestMCPServerOverPipeTransport does, with the logger writing JSON to a
// buffer, and returns the "query" log record it produced along with the
// call's result. It fails if the call logged anything other than exactly one
// such record.
func queryLogRecord(t *testing.T, cfg *Config, pool *Pool, sql string) (map[string]any, *mcp.CallToolResult) {
	t.Helper()
	var buf bytes.Buffer
	server := newMCPServer(cfg, pool, slog.New(slog.NewJSONHandler(&buf, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverT, clientT := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"sql": sql},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		lines = append(lines, rec)
	}
	if len(lines) != 1 || lines[0]["msg"] != "query" {
		t.Fatalf("want exactly one query log line, got:\n%s", buf.String())
	}
	return lines[0], res
}

// unreachableConfig is unitConfig pointed at a loopback port nothing listens
// on: a query that gets past masking fails at the dial, at once, and the
// query line is then written on the error path, which carries the same
// query attribute as the success path.
func unreachableConfig(t *testing.T) *Config {
	t.Helper()
	ln := listen(t)
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()
	cfg := unitConfig()
	cfg.Database.Host = addr.IP.String()
	cfg.Database.Port = addr.Port
	return cfg
}

func attr(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	s, ok := rec[key].(string)
	if !ok {
		t.Fatalf("log record has no string %q: %v", key, rec)
	}
	return s
}

func TestQueryLogScrubsValueShapes(t *testing.T) {
	both := []string{"email", "phone_id"}

	for _, tc := range []struct {
		name  string
		sql   string
		value string // the literal that must not be logged
	}{
		{"email literal", "SELECT 1 FROM users WHERE email = '" + scrubEmail + "'", scrubEmail},
		{"phone literal", "SELECT 1 FROM users WHERE phone = '" + scrubPhone + "'", scrubPhone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := unreachableConfig(t)
			cfg.masker = valuesMasker(t, both)
			rec, _ := queryLogRecord(t, cfg, NewPool(cfg), tc.sql)
			got := attr(t, rec, "query")
			if strings.Contains(got, tc.value) || !strings.Contains(got, maskedValue) {
				t.Errorf("logged query = %q, want %q replaced by %s", got, tc.value, maskedValue)
			}
			want := strings.Replace(tc.sql, tc.value, maskedValue, 1)
			if got != want {
				t.Errorf("logged query = %q, want %q (only the literal changes)", got, want)
			}
		})
	}

	// With value scanning off the line is byte-for-byte what it was: no
	// masking at all, and column rules without detectors, are both off.
	for _, tc := range []struct {
		name   string
		masker *Masker
	}{
		{"no masking", nil},
		{"column rules only", func() *Masker {
			m, err := NewMasker(&MaskingConfig{Mask: []string{"email"}})
			if err != nil {
				t.Fatal(err)
			}
			return m
		}()},
	} {
		t.Run("values off, "+tc.name, func(t *testing.T) {
			sql := "SELECT 1 FROM users WHERE email = '" + scrubEmail + "' OR phone = '" + scrubPhone + "'"
			cfg := unreachableConfig(t)
			cfg.masker = tc.masker
			rec, _ := queryLogRecord(t, cfg, NewPool(cfg), sql)
			if got := attr(t, rec, "query"); got != sql {
				t.Errorf("logged query = %q, want the SQL verbatim %q", got, sql)
			}
		})
	}

	// The error path is the one people forget. A masking refusal for SQL the
	// parser can't read quotes the text near the error, literal included;
	// the database is never reached, so this holds without one.
	t.Run("masking refusal echoing the literal", func(t *testing.T) {
		sql := "SELECT 1 FROM users WHERE email '" + scrubEmail + "'"
		cfg := unreachableConfig(t)
		cfg.masker = valuesMasker(t, both)
		rec, res := queryLogRecord(t, cfg, NewPool(cfg), sql)
		if !res.IsError {
			t.Fatalf("want the call refused, got %+v", res)
		}
		for _, key := range []string{"query", "error"} {
			got := attr(t, rec, key)
			if strings.Contains(got, scrubEmail) || !strings.Contains(got, maskedValue) {
				t.Errorf("logged %s = %q, want the email replaced by %s", key, got, maskedValue)
			}
		}
		if got := attr(t, rec, "error"); !strings.Contains(got, "PII masking refused") {
			t.Errorf("logged error = %q, want the masking refusal", got)
		}
	})
}

// Against a database: the logged query is scrubbed while the statement the
// database ran was the original, which the matching row proves; and an error
// the database itself raises with the literal in it (a syntax error echoes
// the text near the fault) is scrubbed the same way.
func TestQueryLogScrubAgainstDatabase(t *testing.T) {
	both := []string{"email", "phone_id"}

	t.Run("logged query scrubbed, database ran the original", func(t *testing.T) {
		pool := newTestPool(t, func(cfg *Config) { cfg.masker = valuesMasker(t, both) })
		sql := "SELECT id FROM users WHERE email = '" + scrubEmail + "'"
		rec, res := queryLogRecord(t, pool.cfg, pool, sql)
		if res.IsError {
			t.Fatalf("query failed: %+v", res.Content)
		}
		if got, want := attr(t, rec, "query"), strings.Replace(sql, scrubEmail, maskedValue, 1); got != want {
			t.Errorf("logged query = %q, want %q", got, want)
		}
		if rec["level"] != "INFO" {
			t.Errorf("level = %v, want INFO for a query that ran", rec["level"])
		}
		var got struct {
			Rows []map[string]any `json:"rows"`
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("re-encode structured content: %v", err)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode structured content: %v", err)
		}
		if len(got.Rows) != 1 || asNumber(t, got.Rows[0]["id"]) != 1 {
			t.Errorf("rows = %+v, want the one row for %s: the database must see the literal, not the mask", got.Rows, scrubEmail)
		}
	})

	t.Run("database error echoing the literal", func(t *testing.T) {
		// full_access lets a statement the parser can't read through to the
		// database (best effort), which answers with a syntax error quoting
		// the text from the fault onward: the email.
		pool := newTestPool(t, func(cfg *Config) {
			cfg.Mode = modeFullAccess
			m := valuesMasker(t, both)
			m.bestEffort = true
			cfg.masker = m
		})
		sql := "SELECT 1 FROM users WHERE email '" + scrubEmail + "'"
		rec, res := queryLogRecord(t, pool.cfg, pool, sql)
		if !res.IsError {
			t.Fatalf("want a syntax error, got %+v", res)
		}
		got := attr(t, rec, "error")
		if strings.Contains(got, "PII masking refused") {
			t.Fatalf("error = %q, want the database's own error, not a refusal", got)
		}
		if strings.Contains(got, scrubEmail) || !strings.Contains(got, maskedValue) {
			t.Errorf("logged error = %q, want the email replaced by %s", got, maskedValue)
		}
		if q := attr(t, rec, "query"); strings.Contains(q, scrubEmail) {
			t.Errorf("logged query = %q, want the email scrubbed", q)
		}
	})
}
