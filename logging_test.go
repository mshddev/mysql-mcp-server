package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileLoggingConfig returns a loaded config that logs to path.
func fileLoggingConfig(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := LoadConfig(writeConfig(t, validConfig+`
logging:
  output: file
  file: `+path+"\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestNewLoggerWritesJSONToFile(t *testing.T) {
	// The parent directory does not exist yet; newLogger must create it.
	path := filepath.Join(t.TempDir(), "logs", "server.log")
	logger, err := newLogger(fileLoggingConfig(t, path))
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	logger.Info("query", "duration_ms", 42)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var line struct {
		Msg        string `json:"msg"`
		DurationMS int    `json:"duration_ms"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, raw)
	}
	if line.Msg != "query" || line.DurationMS != 42 {
		t.Errorf("log line = %q, want msg=query duration_ms=42", raw)
	}
}

func TestNewLoggerFailsOnUnwritablePath(t *testing.T) {
	// A regular file where a directory is needed makes MkdirAll fail.
	obstacle := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(obstacle, nil, 0o600); err != nil {
		t.Fatalf("write obstacle: %v", err)
	}

	_, err := newLogger(fileLoggingConfig(t, filepath.Join(obstacle, "server.log")))
	if err == nil {
		t.Fatal("newLogger succeeded, want an error for an unwritable path")
	}
	if !strings.Contains(err.Error(), "log") {
		t.Errorf("error %q does not mention the log file", err)
	}
}

func TestNewLoggerHonorsLevel(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfig+"logging:\n  level: warn\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	logger, err := newLogger(cfg)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}

	ctx := context.Background()
	if logger.Enabled(ctx, slog.LevelInfo) {
		t.Error("info is enabled at level warn")
	}
	if !logger.Enabled(ctx, slog.LevelWarn) {
		t.Error("warn is disabled at level warn")
	}
}
