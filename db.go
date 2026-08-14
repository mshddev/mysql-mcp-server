package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

const dialTimeout = 5 * time.Second

// errTruncated aborts result streaming once a cap is hit. The connection is
// discarded afterwards because the rest of the resultset is left unread.
var errTruncated = errors.New("result truncated")

type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
	Note      string   `json:"note,omitempty"`
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

func (p *Pool) dial(ctx context.Context) (*client.Conn, error) {
	conn, err := client.ConnectWithTimeout(p.addr, p.cfg.Database.User,
		p.cfg.Database.Password, p.cfg.Database.Database, dialTimeout)
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
	killer, err := client.ConnectWithTimeout(p.addr, p.cfg.Database.User,
		p.cfg.Database.Password, "", dialTimeout)
	if err != nil {
		return
	}
	defer killer.Close()
	killer.Execute(fmt.Sprintf("KILL QUERY %d", connID))
}

// Query runs one statement and streams the resultset, truncating at the row
// or byte cap. The context deadline is enforced with a server-side KILL.
func (p *Pool) Query(ctx context.Context, sql string) (*QueryResult, error) {
	conn, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	killed := false
	go func() {
		select {
		case <-ctx.Done():
			killed = true
			p.killQuery(conn.GetConnectionID())
		case <-done:
		}
	}()

	res := &QueryResult{Rows: [][]any{}}
	bytesSoFar := 0
	var fields []*mysql.Field
	var streamResult mysql.Result

	err = conn.ExecuteSelectStreaming(sql, &streamResult,
		func(row []mysql.FieldValue) error {
			vals := make([]any, len(row))
			for i := range row {
				v, size := fieldValueToJSON(&row[i], fieldAt(fields, i))
				vals[i] = v
				bytesSoFar += size
			}
			res.Rows = append(res.Rows, vals)
			res.RowCount++
			if res.RowCount >= p.cfg.Limits.MaxRows || bytesSoFar >= p.cfg.Limits.MaxResponseBytes {
				return errTruncated
			}
			return nil
		},
		func(result *mysql.Result) error {
			fields = result.Fields
			for _, f := range fields {
				res.Columns = append(res.Columns, string(f.Name))
			}
			return nil
		})
	close(done)

	switch {
	case err == nil:
		p.release(conn, false)
	case errors.Is(err, errTruncated):
		// Mid-stream abort leaves unread packets; drop the connection.
		p.release(conn, true)
		res.Truncated = true
		res.Note = fmt.Sprintf("truncated at %d rows / %d bytes — narrow the query (add WHERE or LIMIT)",
			res.RowCount, bytesSoFar)
	case killed:
		p.release(conn, true)
		return nil, fmt.Errorf("query exceeded the %ds timeout and was killed", p.cfg.Limits.TimeoutSeconds)
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
		return fv.String(), len(fv.String())
	}
}
