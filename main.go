package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

type QueryInput struct {
	SQL string `json:"sql" jsonschema:"The SQL statement to execute. Read-only: only SELECT/SHOW/DESCRIBE/EXPLAIN will succeed."`
}

func main() {
	configPath := flag.String("config", "./config.yaml", "path to YAML config")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		logger.Error("startup", "error", err.Error())
		os.Exit(1)
	}

	logger, err = newLogger(cfg)
	if err != nil {
		slog.Error("startup", "error", err.Error())
		os.Exit(1)
	}
	// db.go logs through the package-level default, so it must follow the swap.
	slog.SetDefault(logger)

	pool := NewPool(cfg)
	// Fail fast if the database is unreachable or the session setup is rejected.
	probeCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	probe, err := pool.acquire(probeCtx)
	cancel()
	if err != nil {
		logger.Error("startup", "error", "database unreachable: "+err.Error())
		os.Exit(1)
	}
	pool.release(probe, false)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mysql-mcp-server",
		Version: version,
	}, nil)

	description := "Run a read-only SQL query against the MySQL database and get rows back. " +
		"Use SHOW TABLES / DESCRIBE <table> to discover the schema. " +
		"Results are capped; narrow queries with WHERE/LIMIT."
	if cfg.masker != nil {
		description += " Some columns come back as \"<masked>\" under this server's PII policy " +
			"(listed per result in masked_columns); that is intended, do not try to recover the values." +
			" To verify masking the server reads the query, and refuses ones it can't " +
			"check: keep queries straightforward — a SELECT * inside a sub-query, join, or union is " +
			"refused (list the columns instead), and computed columns built from PII are masked."
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: description,
	}, func(ctx context.Context, req *mcp.CallToolRequest, input QueryInput) (*mcp.CallToolResult, *QueryResult, error) {
		queryCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Limits.TimeoutSeconds)*time.Second)
		defer cancel()

		start := time.Now()
		res, err := pool.Query(queryCtx, input.SQL)
		attrs := []any{
			"query", input.SQL,
			"duration_ms", time.Since(start).Milliseconds(),
			"truncated", res != nil && res.Truncated,
		}
		if err != nil {
			logger.Error("query", append(attrs, "error", err.Error())...)
			return nil, nil, err
		}
		logger.Info("query", attrs...)
		return nil, res, nil
	})

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	logger.Info("startup", "listen", cfg.Server.Listen, "database",
		cfg.Database.Host, "masking", cfg.masker != nil, "version", version)
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           bearerAuth(cfg.Server.AuthToken, handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Must outlive the query timeout or responses get cut off mid-write.
		WriteTimeout: time.Duration(cfg.Limits.TimeoutSeconds)*time.Second + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
	err = srv.ListenAndServe()
	logger.Error("shutdown", "error", err.Error())
	os.Exit(1)
}

func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The auth scheme name is case-insensitive per RFC 7235.
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
