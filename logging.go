package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// newLogger builds the logger described by cfg.Logging. For file output it
// verifies the file is writable up front: slog drops handler write errors
// silently, so a bad path would otherwise mean a server that runs without
// logging anything, complaint included.
func newLogger(cfg *Config) (*slog.Logger, error) {
	w := cfg.console()
	if cfg.Logging.Output == "file" {
		if err := os.MkdirAll(filepath.Dir(cfg.Logging.File), 0o755); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		probe, err := os.OpenFile(cfg.Logging.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		probe.Close()
		w = &lumberjack.Logger{
			Filename:   cfg.Logging.File,
			MaxSize:    cfg.Logging.Rotation.MaxSizeMB,
			MaxBackups: cfg.Logging.Rotation.MaxBackups,
			MaxAge:     cfg.Logging.Rotation.MaxAgeDays,
			Compress:   cfg.Logging.Rotation.Compress,
		}
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: cfg.logLevel})), nil
}
