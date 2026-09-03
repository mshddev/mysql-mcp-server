package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at release time via -ldflags "-X main.version=…" (see
// .goreleaser.yaml). Other builds fall back to what Go embeds in the binary:
// the module version for `go install …@vX.Y.Z`, the nearest tag (plus
// "+dirty" or a pseudo-version suffix) for a local `go build` in a checkout,
// and "dev" only when there is no VCS information at all.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}

type QueryInput struct {
	SQL string `json:"sql" jsonschema:"The SQL statement to execute. Read-only: only SELECT/SHOW/DESCRIBE/EXPLAIN will succeed."`
}

// FullAccessQueryInput is QueryInput with an honest schema hint for
// full_access mode. Struct tags are fixed at compile time, so each mode needs
// its own struct.
type FullAccessQueryInput struct {
	SQL string `json:"sql" jsonschema:"The SQL statement to execute. Reads and writes are both allowed; one statement per call, run with autocommit."`
}

func main() {
	configPath := flag.String("config", "./config.yaml", "path to YAML config")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mysql-mcp-server " + version)
		return
	}

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

	// Masking cannot be a guarantee once writes are allowed, and this is the
	// deployer's one reliable notice of that: it prints on every start, where
	// a line in a config file or a doc page only reaches whoever reads it.
	if cfg.masker != nil && cfg.fullAccess() {
		// This one warning always goes to stdout, whatever logging.output and
		// logging.level say: routed through the configured logger it would land
		// in a file nobody opens, or be dropped outright at level error, and the
		// person deploying a write-enabled server has to see it. Written as a
		// JSON line like every other, so a collector reading stdout can still
		// parse the stream. It also goes to the real logger when that writes
		// somewhere else, so a file log keeps the record.
		warn := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		const msg = "masking is best-effort under full_access, not a guarantee"
		const detail = "a write can copy PII into tables the mask rules don't name; " +
			"writes, DDL, and statements the parser can't read run with wire-metadata masking only"
		warn.Warn(msg, "detail", detail)
		if cfg.Logging.Output != "stdout" {
			logger.Warn(msg, "detail", detail)
		}
	}

	pool := NewPool(cfg)
	// Fail fast if the database is unreachable or the session setup is rejected.
	probeCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	probe, err := pool.acquire(probeCtx)
	cancel()
	if err != nil {
		logger.Error("startup", "error", describeConnectError(err))
		os.Exit(1)
	}
	pool.release(probe, false)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mysql-mcp-server",
		Version: version,
	}, nil)

	description := "Run a read-only SQL query against the MySQL database and get rows back. "
	if cfg.fullAccess() {
		description = "Run a SQL statement against the MySQL database. Reads return rows; " +
			"writes (INSERT/UPDATE/DELETE/DDL) are allowed and report affected_rows and last_insert_id. " +
			"Each call is one statement run with autocommit — semicolon batches fail, and session " +
			"state (SET ...) does not persist between calls. "
	}
	description += "Use SHOW TABLES / DESCRIBE <table> to discover the schema. " +
		"Results are capped; narrow queries with WHERE/LIMIT."
	if cfg.masker != nil {
		description += " Some columns come back as \"<masked>\" under this server's PII policy " +
			"(listed per result in masked_columns); that is intended, do not try to recover the values." +
			" To verify masking the server reads the query, and refuses ones it can't " +
			"check: a SELECT * inside a sub-query, join, or union is refused (list the columns " +
			"instead), and computed columns built from PII are masked."
		if cfg.fullAccess() {
			description += " Write statements are not masking-checked."
		}
	}
	run := func(ctx context.Context, sql string) (*mcp.CallToolResult, *QueryResult, error) {
		queryCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Limits.TimeoutSeconds)*time.Second)
		defer cancel()

		start := time.Now()
		res, err := pool.Query(queryCtx, sql)
		attrs := []any{
			"query", sql,
			"duration_ms", time.Since(start).Milliseconds(),
			"truncated", res != nil && res.Truncated,
		}
		if err != nil {
			logger.Error("query", append(attrs, "error", err.Error())...)
			return nil, nil, err
		}
		logger.Info("query", attrs...)
		return nil, res, nil
	}
	tool := &mcp.Tool{Name: "query", Description: description}
	if cfg.fullAccess() {
		mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, input FullAccessQueryInput) (*mcp.CallToolResult, *QueryResult, error) {
			return run(ctx, input.SQL)
		})
	} else {
		mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, input QueryInput) (*mcp.CallToolResult, *QueryResult, error) {
			return run(ctx, input.SQL)
		})
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	logger.Info("startup", "listen", cfg.Server.Listen, "database",
		cfg.Database.Host, "mode", cfg.Mode, "masking", cfg.masker != nil, "version", version)
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           routes(cfg.Server.AuthToken, newHealth(pool.ping), handler),
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
