package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
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
	stdio := flag.Bool("stdio", false, "serve MCP over stdin/stdout for the client that launched this process, instead of listening for HTTP")
	flag.Parse()

	if *showVersion {
		fmt.Println("mysql-mcp-server " + version)
		return
	}

	transport := transportHTTP
	if *stdio {
		transport = transportStdio
	}
	// Nothing but the protocol may touch stdout under stdio, and that holds
	// for the log line reporting a bad config as much as for any other.
	console := os.Stdout
	if *stdio {
		console = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(console, nil))
	slog.SetDefault(logger)

	cfg, err := LoadConfig(*configPath, transport)
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
		// This one warning always goes to the console (stdout, or stderr
		// under stdio), whatever logging.output and logging.level say: routed
		// through the configured logger it would land in a file nobody opens,
		// or be dropped outright at level error, and the person deploying a
		// write-enabled server has to see it. Written as a JSON line like
		// every other, so a collector reading the stream can still parse it.
		// It also goes to the real logger when that writes somewhere else, so
		// a file log keeps the record.
		warn := slog.New(slog.NewJSONHandler(cfg.console(), nil))
		const msg = "masking is best-effort under full_access, not a guarantee"
		const detail = "a write can copy PII into tables the mask rules don't name; " +
			"writes, DDL, and statements the parser can't read run with wire-metadata masking only"
		warn.Warn(msg, "detail", detail)
		if cfg.Logging.Output != "stdout" {
			logger.Warn(msg, "detail", detail)
		}
	}

	if cfg.tlsUnverified() {
		const msg = "database TLS is encrypted but unverified (database.tls.insecure_skip_verify)"
		const detail = "the connection resists eavesdropping, not impersonation; " +
			"anyone on the path who answers as the database is trusted"
		warn := slog.New(slog.NewJSONHandler(cfg.console(), nil))
		warn.Warn(msg, "detail", detail)
		if cfg.Logging.Output != "stdout" {
			logger.Warn(msg, "detail", detail)
		}
	}

	// Nothing listens under stdio, and Server.Listen is left unexpanded
	// there, so the transport check has to come first.
	if !cfg.stdio() && listensBeyondLoopback(cfg.Server.Listen) {
		const msg = "server listens on a non-loopback address"
		const detail = "the token and query results cross the network in plaintext unless a TLS proxy fronts this address; " +
			"bind to 127.0.0.1 with the proxy in front, or keep this on a network you trust"
		warn := slog.New(slog.NewJSONHandler(cfg.console(), nil))
		warn.Warn(msg, "detail", detail, "listen", cfg.Server.Listen)
		if cfg.Logging.Output != "stdout" {
			logger.Warn(msg, "detail", detail, "listen", cfg.Server.Listen)
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

	server := newMCPServer(cfg, pool, logger)

	if cfg.stdio() {
		logger.Info("startup", "transport", transportStdio, "database",
			cfg.Database.Host, "tls", cfg.tlsOn(), "mode", cfg.Mode, "masking", cfg.masker != nil,
			"masking_values", cfg.masker.scansValues(), "version", version)
		// Run returns nil when the client closes our stdin, which is how a
		// stdio session normally ends: the client is done with us.
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			logger.Error("shutdown", "error", err.Error())
			os.Exit(1)
		}
		logger.Info("shutdown", "reason", "client closed the session")
		return
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	logger.Info("startup", "transport", transportHTTP, "listen", cfg.Server.Listen, "database",
		cfg.Database.Host, "tls", cfg.tlsOn(), "mode", cfg.Mode, "masking", cfg.masker != nil,
		"masking_values", cfg.masker.scansValues(), "version", version)
	srv := &http.Server{
		Handler:           routes(cfg.Server.AuthToken, newHealth(pool.ping), handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Must outlive the query timeout or responses get cut off mid-write.
		WriteTimeout: time.Duration(cfg.Limits.TimeoutSeconds)*time.Second + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
	// Bound separately from serving so a port already in use fails here,
	// before any signal handling is armed.
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("startup", "error", err.Error())
		os.Exit(1)
	}
	// systemd stops the unit with SIGTERM; an operator's Ctrl-C is SIGINT.
	// Both drain. stop() is deferred so that once the first signal has been
	// taken a second one falls back to the default action, killing the
	// process outright: the escape hatch when the drain itself is what hangs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serveHTTP(ctx, srv, ln, pool, logger, drainTimeout(cfg)); err != nil {
		logger.Error("shutdown", "error", err.Error())
		os.Exit(1)
	}
}

// maxDrain caps how long a stopping server waits for in-flight requests.
// systemd sends SIGKILL after TimeoutStopSec, 90s by default, and the pool
// close and process exit need a little of that; a drain that could run past
// it would only trade a clean stop for a kill anyway.
const maxDrain = 80 * time.Second

// drainTimeout gives every in-flight query room to reach its own deadline,
// where pool.Query's watcher kills it server-side, plus margin for the
// response to be written. Only past the cap can a statement outlive the
// process; the session-level statement timeout then ends it on MariaDB, and
// on MySQL only when it is a SELECT (see setupSession).
func drainTimeout(cfg *Config) time.Duration {
	return min(time.Duration(cfg.Limits.TimeoutSeconds)*time.Second+5*time.Second, maxDrain)
}

// serveHTTP serves srv on ln until it fails or ctx is done. A done ctx means
// a stop was requested: the listener closes, requests already in progress
// get up to drain to finish, and the pool's idle connections are closed so
// the database sees an orderly hangup rather than dropped sockets. It
// returns nil for that path — a requested stop is not an error — and the
// serve error for the other.
func serveHTTP(ctx context.Context, srv *http.Server, ln net.Listener, pool *Pool, logger *slog.Logger, drain time.Duration) error {
	// Buffered so the goroutine can exit even when nobody reads it, which is
	// the shutdown path: Serve returns ErrServerClosed after the select has
	// moved on.
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	// NotifyContext names the signal in the cause; a test cancels with
	// whatever reason it likes.
	logger.Info("shutdown", "reason", context.Cause(ctx).Error(), "drain", drain.String())
	// A fresh context: the one that fired is already done and would end
	// Shutdown at once.
	drainCtx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		// Requests still running past the bound are abandoned with the
		// process; the note says so, since the query log will show them
		// started and never finished.
		logger.Warn("shutdown", "error", "drain expired with requests still in flight: "+err.Error())
	}
	pool.Close()
	return nil
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

// toolAnnotations tells a client what the query tool can do to the database,
// which is the mode's decision, not the tool's: under read_only every call is
// a read the session can't turn into a write, and repeating one changes
// nothing; under full_access a single statement can drop a table, and running
// it twice is not the same as once. OpenWorldHint is false either way, the
// tool talks to one database. The hints are advisory by spec, so they say
// only what the mode enforces.
func toolAnnotations(cfg *Config) *mcp.ToolAnnotations {
	yes, no := true, false
	if cfg.fullAccess() {
		return &mcp.ToolAnnotations{
			Title:           "MySQL Query",
			ReadOnlyHint:    false,
			DestructiveHint: &yes,
			IdempotentHint:  false,
			OpenWorldHint:   &no,
		}
	}
	return &mcp.ToolAnnotations{
		Title:           "MySQL Query",
		ReadOnlyHint:    true,
		DestructiveHint: &no,
		IdempotentHint:  true,
		OpenWorldHint:   &no,
	}
}

// newMCPServer builds the MCP server with its one tool wired to pool. The
// result is transport-agnostic: main hands it to the HTTP handler or runs it
// over stdio.
func newMCPServer(cfg *Config, pool *Pool, logger *slog.Logger) *mcp.Server {
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
		if cfg.masker.scansValues() {
			description += " Text that looks like personal data (an email address, a phone number) is also" +
				" masked wherever it appears inside a value (listed per result in masked_values)."
		}
		if cfg.masker.scansJSON() {
			description += " Inside a JSON-valued cell, a key named like a masked column has its whole" +
				" value masked too (listed per result in masked_json_keys)."
		}
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
	tool := &mcp.Tool{Name: "query", Description: description, Annotations: toolAnnotations(cfg)}
	if cfg.fullAccess() {
		mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, input FullAccessQueryInput) (*mcp.CallToolResult, *QueryResult, error) {
			return run(ctx, input.SQL)
		})
	} else {
		mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, input QueryInput) (*mcp.CallToolResult, *QueryResult, error) {
			return run(ctx, input.SQL)
		})
	}
	return server
}
