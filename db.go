package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

const dialTimeout = 5 * time.Second

// errTruncated aborts result streaming once a cap is hit. The connection is
// discarded afterwards because the rest of the resultset is left unread.
var errTruncated = errors.New("result truncated")

type QueryResult struct {
	// Columns preserves SELECT order; the row objects can't, because JSON
	// object keys serialize alphabetically.
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
	Note      string           `json:"note,omitempty"`
}

type Pool struct {
	cfg  *Config
	addr string
	sem  chan struct{}     // caps concurrent queries at MaxConnections
	idle chan *client.Conn // reusable connections, capacity MaxConnections
}

func NewPool(cfg *Config) *Pool {
	n := cfg.Limits.MaxConnections
	return &Pool{
		cfg:  cfg,
		addr: fmt.Sprintf("%s:%d", cfg.Database.Host, cfg.Database.Port),
		sem:  make(chan struct{}, n),
		idle: make(chan *client.Conn, n),
	}
}

// socketDeadlines bounds every read/write so a wedged server or half-dead
// network can never block a pool slot forever. The deadline resets per packet,
// so it acts as an idle timeout and must stay above the query timeout.
func (p *Pool) socketDeadlines(d time.Duration) client.Option {
	return func(c *client.Conn) error {
		c.ReadTimeout = d
		c.WriteTimeout = d
		return nil
	}
}

func (p *Pool) dial(ctx context.Context) (*client.Conn, error) {
	d := time.Duration(p.cfg.Limits.TimeoutSeconds)*time.Second + 10*time.Second
	// ConnectWithTimeout ignores its timeout argument (go-mysql v1.16.0
	// hardcodes 10s); the context form is the one that actually bounds a dial.
	conn, err := client.ConnectWithContext(ctx, p.addr, p.cfg.Database.User,
		p.cfg.Database.Password, p.cfg.Database.Database, dialTimeout, p.socketDeadlines(d))
	if err != nil {
		return nil, err
	}
	if err := p.setupSession(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// setupSession hardens every pooled connection: read-only transactions plus a
// server-side statement timeout as backup for the client-side kill. MariaDB
// and MySQL spell the timeout variable differently.
func (p *Pool) setupSession(conn *client.Conn) error {
	stmts := []string{
		"SET NAMES utf8mb4",
		"SET SESSION TRANSACTION READ ONLY",
	}
	timeout := p.cfg.Limits.TimeoutSeconds
	if strings.Contains(strings.ToLower(conn.GetServerVersion()), "mariadb") {
		stmts = append(stmts, fmt.Sprintf("SET SESSION max_statement_time=%d", timeout))
	} else {
		stmts = append(stmts, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d", timeout*1000))
	}
	for _, s := range stmts {
		if _, err := conn.Execute(s); err != nil {
			return fmt.Errorf("session setup %q: %w", s, err)
		}
	}
	return nil
}

func (p *Pool) acquire(ctx context.Context) (*client.Conn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("timed out waiting for a free connection (%d in use)", p.cfg.Limits.MaxConnections)
	}
	for {
		select {
		case conn := <-p.idle:
			if conn.Ping() == nil {
				return conn, nil
			}
			conn.Close()
		default:
			conn, err := p.dial(ctx)
			if err != nil {
				<-p.sem
				return nil, err
			}
			return conn, nil
		}
	}
}

func (p *Pool) release(conn *client.Conn, broken bool) {
	if broken {
		conn.Close()
	} else {
		select {
		case p.idle <- conn:
		default:
			conn.Close()
		}
	}
	<-p.sem
}

// killQuery interrupts a running statement from a dedicated short-lived
// connection; abandoning the client side alone would leave the query running
// on the server.
func (p *Pool) killQuery(connID uint32) {
	// Fresh context on purpose: the query ctx that triggered the kill is
	// already cancelled and would abort the dial immediately.
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	killer, err := client.ConnectWithContext(ctx, p.addr, p.cfg.Database.User,
		p.cfg.Database.Password, "", dialTimeout, p.socketDeadlines(dialTimeout))
	if err != nil {
		slog.Warn("kill_query", "conn_id", connID, "error", err.Error())
		return
	}
	defer killer.Close()
	if _, err := killer.Execute(fmt.Sprintf("KILL QUERY %d", connID)); err != nil {
		slog.Warn("kill_query", "conn_id", connID, "error", err.Error())
	}
}

// Query runs one statement and streams the resultset, truncating at the row
// or byte cap. The context deadline is enforced with a server-side KILL.
func (p *Pool) Query(ctx context.Context, sql string) (*QueryResult, error) {
	conn, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}

	// The watcher must be joined before deciding the connection's fate: if the
	// deadline and completion race, a KILL may target the connection after it
	// would have been reused for someone else's query.
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	var killed atomic.Bool
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			killed.Store(true)
			p.killQuery(conn.GetConnectionID())
		case <-done:
		}
	}()

	res := &QueryResult{Columns: []string{}, Rows: []map[string]any{}}
	bytesSoFar := 0
	var fields []*mysql.Field
	var streamResult mysql.Result

	err = conn.ExecuteSelectStreaming(sql, &streamResult,
		func(row []mysql.FieldValue) error {
			vals := make(map[string]any, len(row))
			for i := range row {
				v, size := fieldValueToJSON(&row[i], fieldAt(fields, i))
				key := labelAt(res.Columns, i)
				vals[key] = v
				bytesSoFar += size + len(key) + 4 // the key is repeated per row
			}
			res.Rows = append(res.Rows, vals)
			if bytesSoFar >= p.cfg.Limits.MaxResponseBytes {
				return errTruncated
			}
			return nil
		},
		func(result *mysql.Result) error {
			fields = result.Fields
			res.Columns = columnLabels(fields)
			return nil
		})
	close(done)
	<-watcherDone

	switch {
	case err == nil:
		// A statement answered with an OK packet instead of a resultset (SET,
		// USE, DO, ...) may have changed session state — read-only mode, the
		// statement timeout, the default schema — so it must never be handed
		// to the next caller. Also discard if a kill fired after completion
		// (deadline lost the race), since the KILL may still land.
		p.release(conn, killed.Load() || fields == nil)
	case errors.Is(err, errTruncated):
		// Mid-stream abort leaves unread packets; drop the connection.
		p.release(conn, true)
		res.Truncated = true
		res.Note = fmt.Sprintf("truncated at ~%d bytes — narrow the query (add WHERE or LIMIT)", bytesSoFar)
	case killed.Load():
		p.release(conn, true)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("query exceeded the %ds timeout and was killed", p.cfg.Limits.TimeoutSeconds)
		}
		return nil, errors.New("query canceled by the caller and killed")
	default:
		p.release(conn, true)
		return nil, err
	}
	return res, nil
}

func fieldAt(fields []*mysql.Field, i int) *mysql.Field {
	if i < len(fields) {
		return fields[i]
	}
	return nil
}

func labelAt(labels []string, i int) string {
	if i < len(labels) {
		return labels[i]
	}
	return fmt.Sprintf("column_%d", i+1)
}

// columnLabels turns resultset metadata into unique row-object keys. The base
// label is the column name exactly as written in the query (aliases
// respected). Duplicates are qualified with the table alias from the query
// (u.id, b.id); anything still colliding gets a numeric suffix.
func columnLabels(fields []*mysql.Field) []string {
	labels := make([]string, len(fields))
	counts := make(map[string]int, len(fields))
	for i, f := range fields {
		labels[i] = string(f.Name)
		counts[labels[i]]++
	}
	for i, f := range fields {
		if counts[labels[i]] > 1 && len(f.Table) > 0 {
			labels[i] = string(f.Table) + "." + labels[i]
		}
	}
	// First occurrence of each label keeps its name; later ones get bumped to
	// a free suffix. Reserving all first occurrences up front stops a bumped
	// duplicate from stealing the name of a genuine column further right.
	used := make(map[string]bool, len(labels))
	dup := make([]bool, len(labels))
	for i, l := range labels {
		dup[i] = used[l]
		used[l] = true
	}
	for i, l := range labels {
		if !dup[i] {
			continue
		}
		final := l
		for n := 2; used[final]; n++ {
			final = fmt.Sprintf("%s_%d", l, n)
		}
		used[final] = true
		labels[i] = final
	}
	return labels
}

// Column types that hold real binary payloads when the charset is binary(63).
// Temporal types also report charset 63 but arrive as readable text, so they
// are deliberately not listed.
func isBinaryField(f *mysql.Field) bool {
	if f == nil || f.Charset != 63 {
		return false
	}
	switch f.Type {
	case mysql.MYSQL_TYPE_TINY_BLOB, mysql.MYSQL_TYPE_MEDIUM_BLOB,
		mysql.MYSQL_TYPE_LONG_BLOB, mysql.MYSQL_TYPE_BLOB,
		mysql.MYSQL_TYPE_VAR_STRING, mysql.MYSQL_TYPE_STRING,
		mysql.MYSQL_TYPE_GEOMETRY, mysql.MYSQL_TYPE_BIT:
		return true
	}
	return false
}

// fieldValueToJSON converts one cell to a JSON-safe value and returns an
// approximate serialized size for the byte cap.
func fieldValueToJSON(fv *mysql.FieldValue, f *mysql.Field) (any, int) {
	switch fv.Type {
	case mysql.FieldValueTypeNull:
		return nil, 4
	case mysql.FieldValueTypeSigned:
		return fv.AsInt64(), 8
	case mysql.FieldValueTypeUnsigned:
		return fv.AsUint64(), 8
	case mysql.FieldValueTypeFloat:
		return fv.AsFloat64(), 8
	case mysql.FieldValueTypeString:
		b := fv.AsString()
		if isBinaryField(f) {
			return fmt.Sprintf("<binary, %d bytes>", len(b)), 24
		}
		return string(b), len(b) + 2
	default:
		s := fv.String()
		return s, len(s)
	}
}
