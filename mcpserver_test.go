package main

import (
	"context"
	"encoding/json"
	"log/slog"
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
