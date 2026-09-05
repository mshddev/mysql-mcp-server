package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio path hands the server to mcp.Server.Run over a pipe transport;
// the SDK's in-memory transport is the same connection type minus the file
// descriptors, so a full initialize / tools/list / tools/call round-trip here
// covers everything but the plumbing to the real stdin and stdout.
func TestMCPServerOverPipeTransport(t *testing.T) {
	pool := newTestPool(t, nil)
	cfg := pool.cfg
	cfg.transport = transportStdio
	server := newMCPServer(cfg, pool, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serverT, clientT := mcp.NewInMemoryTransports()
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "query" {
		t.Fatalf("tools = %+v, want exactly one named query", tools.Tools)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"sql": "SELECT 1 AS one"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tools/call returned an error result: %+v", res.Content)
	}
	var got struct {
		Rows []map[string]any `json:"rows"`
	}
	// The client decodes structuredContent into generic JSON; round-trip it
	// into the shape the test cares about.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-encode structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if len(got.Rows) != 1 || asNumber(t, got.Rows[0]["one"]) != 1 {
		t.Errorf("rows = %+v, want one row with one=1", got.Rows)
	}

	// Closing the client side is how a stdio session ends; Run must return
	// cleanly so the process exits 0 rather than logging a spurious error.
	if err := session.Close(); err != nil {
		t.Fatalf("client close: %v", err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("server.Run returned %v after the client hung up, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server.Run did not return after the client closed the session")
	}
}

// The annotations are read straight off the wire rather than from the
// client's decoded struct: a bare-bool hint decoded as false looks the same
// whether the server sent false or nothing, and the point is that it sends
// all five, honestly, in both modes.
func TestToolAnnotationsPerMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want map[string]any
	}{
		{modeReadOnly, map[string]any{
			"title": "MySQL Query", "readOnlyHint": true, "destructiveHint": false,
			"idempotentHint": true, "openWorldHint": false,
		}},
		{modeFullAccess, map[string]any{
			"title": "MySQL Query", "readOnlyHint": false, "destructiveHint": true,
			"idempotentHint": false, "openWorldHint": false,
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg := unitConfig()
			cfg.Mode = tc.mode
			tools := listToolsOverHTTP(t, cfg)
			if len(tools) != 1 || tools[0]["name"] != "query" {
				t.Fatalf("tools = %v, want exactly one named query", tools)
			}
			got, ok := tools[0]["annotations"].(map[string]any)
			if !ok {
				t.Fatalf("tool has no annotations object: %v", tools[0])
			}
			for k, want := range tc.want {
				if v, present := got[k]; !present {
					t.Errorf("%s missing from the wire; annotations = %v", k, got)
				} else if v != want {
					t.Errorf("%s = %v, want %v", k, v, want)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("annotations = %v, want exactly the keys %v", got, tc.want)
			}
		})
	}
}

// listToolsOverHTTP sends a cold tools/list through the same handler chain
// main serves, and returns the tools array as generic JSON.
func listToolsOverHTTP(t *testing.T, cfg *Config) []map[string]any {
	t.Helper()
	server := newMCPServer(cfg, NewPool(cfg), slog.New(slog.DiscardHandler))
	handler := routes(cfg.Server.AuthToken, newHealth(func(context.Context) error { return nil }),
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: true}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+cfg.Server.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list: status %d, body %s", rec.Code, rec.Body.String())
	}

	// The stateless handler may answer as plain JSON or as one SSE event.
	body := rec.Body.String()
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		var data []string
		for _, line := range strings.Split(body, "\n") {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimSpace(rest))
			}
		}
		body = strings.Join(data, "")
	}
	var msg struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode tools/list response %q: %v", body, err)
	}
	if msg.Error != nil {
		t.Fatalf("tools/list error: %s", msg.Error.Message)
	}
	return msg.Result.Tools
}
