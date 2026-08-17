package main

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// ---------------------------------------------------------------------------
// Unit tests: no database needed.
// ---------------------------------------------------------------------------

func TestIsBinaryField(t *testing.T) {
	const binaryCharset = 63
	const utf8Charset = 33

	tests := []struct {
		name  string
		field *mysql.Field
		want  bool
	}{
		{name: "nil field", field: nil, want: false},

		{name: "binary blob", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_BLOB}, want: true},
		{name: "binary tiny blob", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_TINY_BLOB}, want: true},
		{name: "binary medium blob", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_MEDIUM_BLOB}, want: true},
		{name: "binary long blob", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_LONG_BLOB}, want: true},
		{name: "varbinary", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_VAR_STRING}, want: true},
		{name: "binary string", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_STRING}, want: true},
		{name: "geometry", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_GEOMETRY}, want: true},
		{name: "bit", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_BIT}, want: true},

		// Regression: temporal columns report the binary charset but carry
		// readable text, so masking them would hide ordinary timestamps.
		{name: "datetime", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_DATETIME}, want: false},
		{name: "timestamp", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_TIMESTAMP}, want: false},
		{name: "date", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_DATE}, want: false},
		{name: "newdate", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_NEWDATE}, want: false},
		{name: "time", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_TIME}, want: false},
		{name: "decimal", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_NEWDECIMAL}, want: false},
		{name: "long", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_LONG}, want: false},
		{name: "json", field: &mysql.Field{Charset: binaryCharset, Type: mysql.MYSQL_TYPE_JSON}, want: false},

		// Same column types, but a text charset: plain strings.
		{name: "utf8 blob is text", field: &mysql.Field{Charset: utf8Charset, Type: mysql.MYSQL_TYPE_BLOB}, want: false},
		{name: "utf8 varchar", field: &mysql.Field{Charset: utf8Charset, Type: mysql.MYSQL_TYPE_VAR_STRING}, want: false},
		{name: "utf8 char", field: &mysql.Field{Charset: utf8Charset, Type: mysql.MYSQL_TYPE_STRING}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBinaryField(tt.field); got != tt.want {
				t.Errorf("isBinaryField() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFieldValueToJSON(t *testing.T) {
	binaryBlob := &mysql.Field{Charset: 63, Type: mysql.MYSQL_TYPE_BLOB}
	textColumn := &mysql.Field{Charset: 33, Type: mysql.MYSQL_TYPE_VAR_STRING}
	datetime := &mysql.Field{Charset: 63, Type: mysql.MYSQL_TYPE_DATETIME}

	tests := []struct {
		name     string
		value    mysql.FieldValue
		field    *mysql.Field
		want     any
		wantSize int
	}{
		{
			name:     "null",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeNull, 0, nil),
			field:    textColumn,
			want:     nil,
			wantSize: 4,
		},
		{
			name:     "null in a binary column",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeNull, 0, nil),
			field:    binaryBlob,
			want:     nil,
			wantSize: 4,
		},
		{
			name:     "signed positive",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(42), nil),
			field:    nil,
			want:     int64(42),
			wantSize: 8,
		},
		{
			name:     "signed negative",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(math.MaxUint64), nil), // -1
			field:    nil,
			want:     int64(-1),
			wantSize: 8,
		},
		{
			// Within the float64-safe range the unsigned path stays a number;
			// a value above MaxInt64 is the only way to tell it from the signed
			// path.
			name:     "unsigned above MaxInt64 but float-safe",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeUnsigned, uint64(maxSafeInteger), nil),
			field:    nil,
			want:     uint64(maxSafeInteger),
			wantSize: 8,
		},
		{
			// Past 2^53 a float64 can't round-trip the value, so it goes out as a
			// string to survive the SDK's marshalling.
			name:     "signed above the safe range is a string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(math.MaxInt64), nil),
			field:    nil,
			want:     "9223372036854775807",
			wantSize: len("9223372036854775807") + 2,
		},
		{
			name:     "signed below the safe range is a string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(math.MaxInt64+1), nil), // math.MinInt64
			field:    nil,
			want:     "-9223372036854775808",
			wantSize: len("-9223372036854775808") + 2,
		},
		{
			name:     "unsigned above the safe range is a string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeUnsigned, uint64(math.MaxUint64), nil),
			field:    nil,
			want:     "18446744073709551615",
			wantSize: len("18446744073709551615") + 2,
		},
		{
			// Exactly 2^53 is still exact in a float64, so it stays a number.
			name:     "signed at the safe boundary stays a number",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(maxSafeInteger), nil),
			field:    nil,
			want:     int64(maxSafeInteger),
			wantSize: 8,
		},
		{
			name:     "signed just past the boundary is a string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeSigned, uint64(maxSafeInteger+1), nil),
			field:    nil,
			want:     "9007199254740993",
			wantSize: len("9007199254740993") + 2,
		},
		{
			name:     "float",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeFloat, math.Float64bits(3.5), nil),
			field:    nil,
			want:     3.5,
			wantSize: 8,
		},
		{
			name:     "negative float",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeFloat, math.Float64bits(-0.125), nil),
			field:    nil,
			want:     -0.125,
			wantSize: 8,
		},
		{
			name:     "string stays a string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte("hello")),
			field:    textColumn,
			want:     "hello",
			wantSize: len("hello") + 2,
		},
		{
			name:     "string with a nil field",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte("hello")),
			field:    nil,
			want:     "hello",
			wantSize: len("hello") + 2,
		},
		{
			name:     "empty string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte("")),
			field:    textColumn,
			want:     "",
			wantSize: 2,
		},
		{
			name:     "multibyte string",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte("héllo ✓")),
			field:    textColumn,
			want:     "héllo ✓",
			wantSize: len("héllo ✓") + 2,
		},
		{
			name:     "binary payload is masked",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A}),
			field:    binaryBlob,
			want:     "<binary, 6 bytes>",
			wantSize: 24,
		},
		{
			name:     "empty binary payload is masked",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte{}),
			field:    binaryBlob,
			want:     "<binary, 0 bytes>",
			wantSize: 24,
		},
		{
			// Regression: a DATETIME also reports charset 63 but must come
			// through as its text.
			name:     "datetime is not masked",
			value:    mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte("2026-08-14 09:30:00")),
			field:    datetime,
			want:     "2026-08-14 09:30:00",
			wantSize: len("2026-08-14 09:30:00") + 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fv := tt.value
			got, size := fieldValueToJSON(&fv, tt.field)
			if got != tt.want {
				t.Errorf("value = %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
			if size != tt.wantSize {
				t.Errorf("size = %d, want %d", size, tt.wantSize)
			}
		})
	}
}

// The byte cap is only useful if a big cell reports a big size.
func TestFieldValueToJSONSizeTracksPayload(t *testing.T) {
	big := strings.Repeat("x", 10_000)
	fv := mysql.NewFieldValue(mysql.FieldValueTypeString, 0, []byte(big))

	_, size := fieldValueToJSON(&fv, &mysql.Field{Charset: 33, Type: mysql.MYSQL_TYPE_VAR_STRING})
	if size < len(big) {
		t.Errorf("size = %d, want at least %d", size, len(big))
	}

	// A masked binary blob is reported at its placeholder size, not the size of
	// the payload it replaces.
	_, maskedSize := fieldValueToJSON(&fv, &mysql.Field{Charset: 63, Type: mysql.MYSQL_TYPE_BLOB})
	if maskedSize > 64 {
		t.Errorf("masked size = %d, want a small placeholder size", maskedSize)
	}
}

func TestFieldAt(t *testing.T) {
	a := &mysql.Field{Name: []byte("a")}
	b := &mysql.Field{Name: []byte("b")}
	fields := []*mysql.Field{a, b}

	if got := fieldAt(fields, 0); got != a {
		t.Errorf("fieldAt(0) = %v, want %v", got, a)
	}
	if got := fieldAt(fields, 1); got != b {
		t.Errorf("fieldAt(1) = %v, want %v", got, b)
	}
	// Out of range must yield nil rather than panic; fieldValueToJSON treats a
	// nil field as "not binary".
	if got := fieldAt(fields, 2); got != nil {
		t.Errorf("fieldAt(2) = %v, want nil", got)
	}
	if got := fieldAt(nil, 0); got != nil {
		t.Errorf("fieldAt(nil, 0) = %v, want nil", got)
	}
}

func TestColumnLabels(t *testing.T) {
	field := func(name, table string) *mysql.Field {
		return &mysql.Field{Name: []byte(name), Table: []byte(table)}
	}

	tests := []struct {
		name   string
		fields []*mysql.Field
		want   []string
	}{
		{name: "nil fields", fields: nil, want: []string{}},
		{
			name:   "unique names stay bare",
			fields: []*mysql.Field{field("id", "u"), field("name", "u")},
			want:   []string{"id", "name"},
		},
		{
			name:   "join duplicates get table-qualified",
			fields: []*mysql.Field{field("id", "u"), field("id", "b"), field("name", "u")},
			want:   []string{"u.id", "b.id", "name"},
		},
		{
			name:   "duplicates without a table get suffixed",
			fields: []*mysql.Field{field("x", ""), field("x", ""), field("x", "")},
			want:   []string{"x", "x_2", "x_3"},
		},
		{
			// Suffixing "x" to "x_2" must not collide with a real "x_2" column.
			name:   "suffix collides with an existing label",
			fields: []*mysql.Field{field("x", ""), field("x", ""), field("x_2", "")},
			want:   []string{"x", "x_3", "x_2"},
		},
		{
			// Same table selected twice without distinct aliases: qualifying
			// does not help, so the suffix pass must resolve it.
			name:   "qualified labels still colliding",
			fields: []*mysql.Field{field("id", "u"), field("id", "u")},
			want:   []string{"u.id", "u.id_2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := columnLabels(tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("columnLabels() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("label %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLabelAt(t *testing.T) {
	labels := []string{"id", "name"}
	if got := labelAt(labels, 1); got != "name" {
		t.Errorf("labelAt(1) = %q, want %q", got, "name")
	}
	// Out of range must yield a stable placeholder rather than panic.
	if got := labelAt(labels, 2); got != "column_3" {
		t.Errorf("labelAt(2) = %q, want %q", got, "column_3")
	}
}

func TestNewPoolSizesChannels(t *testing.T) {
	cfg := &Config{}
	cfg.Database.Host = "db.internal"
	cfg.Database.Port = 3307
	cfg.Limits.MaxConnections = 3

	p := NewPool(cfg)
	if p.addr != "db.internal:3307" {
		t.Errorf("addr = %q, want db.internal:3307", p.addr)
	}
	if cap(p.sem) != 3 || cap(p.idle) != 3 {
		t.Errorf("sem cap = %d, idle cap = %d, want 3 and 3", cap(p.sem), cap(p.idle))
	}
}

// ---------------------------------------------------------------------------
// Integration tests: skipped unless a database is reachable.
// ---------------------------------------------------------------------------

// Set MYSQL_TEST_ADDR (e.g. 127.0.0.1:3306) to run these against a database
// seeded with seed/seed.sql. The user, password and schema can be overridden
// with MYSQL_TEST_USER, MYSQL_TEST_PASSWORD and MYSQL_TEST_DATABASE.
const addrEnv = "MYSQL_TEST_ADDR"

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// testConfig skips the test unless a database address is configured. The
// limits are deliberately generous; each test tightens the ones it exercises.
func testConfig(t *testing.T) *Config {
	t.Helper()

	addr := os.Getenv(addrEnv)
	if addr == "" {
		t.Skipf("set %s (e.g. 127.0.0.1:3306) to run database tests", addrEnv)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q is not host:port: %v", addrEnv, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("%s=%q has a non-numeric port: %v", addrEnv, addr, err)
	}

	cfg := &Config{}
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.AuthToken = "test-token"
	cfg.Database.Host = host
	cfg.Database.Port = port
	cfg.Database.Username = envOr("MYSQL_TEST_USER", "mcp_readonly")
	cfg.Database.Password = envOr("MYSQL_TEST_PASSWORD", "devpassword")
	cfg.Database.DBName = envOr("MYSQL_TEST_DATABASE", "mcp_dev")
	cfg.Limits.TimeoutSeconds = 10
	cfg.Limits.MaxResponseBytes = 8 << 20
	cfg.Limits.MaxConnections = 4
	return cfg
}

// newTestPool builds a pool from testConfig, letting the caller adjust the
// limits, and closes any pooled connections when the test ends.
func newTestPool(t *testing.T, adjust func(*Config)) *Pool {
	t.Helper()

	cfg := testConfig(t)
	if adjust != nil {
		adjust(cfg)
	}
	p := NewPool(cfg)
	t.Cleanup(func() {
		for {
			select {
			case conn := <-p.idle:
				conn.Close()
			default:
				return
			}
		}
	})
	return p
}

func query(t *testing.T, p *Pool, sql string) (*QueryResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(p.cfg.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	return p.Query(ctx, sql)
}

func mustQuery(t *testing.T, p *Pool, sql string) *QueryResult {
	t.Helper()
	res, err := query(t, p, sql)
	if err != nil {
		t.Fatalf("Query(%q): %v", sql, err)
	}
	return res
}

// asNumber accepts whichever numeric shape the driver produced for a column.
func asNumber(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case float64:
		return n
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			t.Fatalf("value %q is not a number: %v", n, err)
		}
		return f
	default:
		t.Fatalf("value %#v (%T) is not a number", v, v)
		return 0
	}
}

func connectionID(t *testing.T, p *Pool) float64 {
	t.Helper()
	res := mustQuery(t, p, "SELECT CONNECTION_ID() AS id")
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("CONNECTION_ID() returned %v, want one row of one column", res.Rows)
	}
	return asNumber(t, res.Rows[0]["id"])
}

func TestQueryBasicResult(t *testing.T) {
	p := newTestPool(t, nil)

	res := mustQuery(t, p, "SELECT id, name, email, phone, avatar, created_at FROM users ORDER BY id")

	wantColumns := []string{"id", "name", "email", "phone", "avatar", "created_at"}
	if len(res.Columns) != len(wantColumns) {
		t.Fatalf("columns = %v, want %v", res.Columns, wantColumns)
	}
	for i, want := range wantColumns {
		if res.Columns[i] != want {
			t.Errorf("column %d = %q, want %q", i, res.Columns[i], want)
		}
	}
	if len(res.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(res.Rows))
	}
	if res.Truncated || res.Note != "" {
		t.Errorf("Truncated = %v, Note = %q, want false and empty", res.Truncated, res.Note)
	}

	first := res.Rows[0]
	if got := asNumber(t, first["id"]); got != 1 {
		t.Errorf("id = %v, want 1", got)
	}
	if first["name"] != "Andi Wijaya" {
		t.Errorf("name = %#v, want %q", first["name"], "Andi Wijaya")
	}
	if first["email"] != "andi@example.com" {
		t.Errorf("email = %#v, want %q", first["email"], "andi@example.com")
	}
	// The seeded avatar is a 6-byte BLOB and must be masked, not returned raw.
	if first["avatar"] != "<binary, 6 bytes>" {
		t.Errorf("avatar = %#v, want %q", first["avatar"], "<binary, 6 bytes>")
	}
	// Regression: DATETIME also reports the binary charset but is readable text.
	created, ok := first["created_at"].(string)
	if !ok || !strings.Contains(created, "-") {
		t.Errorf("created_at = %#v, want a readable timestamp string", first["created_at"])
	}

	// NULLs come through as nil, in text and binary columns alike. A key must
	// still be present for a NULL cell, not omitted.
	second := res.Rows[1]
	if v, present := second["phone"]; !present || v != nil {
		t.Errorf("phone of row 2 = %#v (present %v), want an explicit nil", v, present)
	}
	if v, present := second["avatar"]; !present || v != nil {
		t.Errorf("avatar of row 2 = %#v (present %v), want an explicit nil", v, present)
	}
	if res.Rows[2]["email"] != nil {
		t.Errorf("email of row 3 = %#v, want nil", res.Rows[2]["email"])
	}
}

func TestQueryValueShapes(t *testing.T) {
	p := newTestPool(t, nil)

	res := mustQuery(t, p, "SELECT price FROM bookings ORDER BY id LIMIT 1")
	// DECIMAL is a binary-charset column that must stay readable.
	if price, ok := res.Rows[0]["price"].(string); !ok || price != "1500000.00" {
		t.Errorf("price = %#v, want the string %q", res.Rows[0]["price"], "1500000.00")
	}

	res = mustQuery(t, p,
		"SELECT 7 AS pos, -7 AS neg, CAST(3.5 AS DOUBLE) AS dbl, 'text' AS str, NULL AS missing")
	row := res.Rows[0]
	if row["pos"] != int64(7) {
		t.Errorf("literal 7 = %#v (%T), want int64(7)", row["pos"], row["pos"])
	}
	if row["neg"] != int64(-7) {
		t.Errorf("literal -7 = %#v (%T), want int64(-7)", row["neg"], row["neg"])
	}
	if row["dbl"] != 3.5 {
		t.Errorf("double = %#v (%T), want float64(3.5)", row["dbl"], row["dbl"])
	}
	if row["str"] != "text" {
		t.Errorf("string = %#v, want %q", row["str"], "text")
	}
	if row["missing"] != nil {
		t.Errorf("NULL = %#v, want nil", row["missing"])
	}
}

func TestQueryDuplicateColumnNames(t *testing.T) {
	p := newTestPool(t, nil)

	// A join with two id columns: without disambiguation one would silently
	// overwrite the other in the row object.
	res := mustQuery(t, p,
		"SELECT u.id, b.id FROM users u JOIN bookings b ON b.user_id = u.id ORDER BY b.id LIMIT 1")
	want := []string{"u.id", "b.id"}
	for i, w := range want {
		if res.Columns[i] != w {
			t.Errorf("column %d = %q, want %q", i, res.Columns[i], w)
		}
	}
	row := res.Rows[0]
	if len(row) != 2 {
		t.Fatalf("row has %d keys (%v), want both id columns", len(row), row)
	}
	if _, ok := row["u.id"]; !ok {
		t.Errorf("row = %v, want a %q key", row, "u.id")
	}
	if _, ok := row["b.id"]; !ok {
		t.Errorf("row = %v, want a %q key", row, "b.id")
	}

	// Duplicate aliases with no table to qualify by fall back to suffixes.
	res = mustQuery(t, p, "SELECT 1 AS x, 2 AS x")
	row = res.Rows[0]
	if asNumber(t, row["x"]) != 1 || asNumber(t, row["x_2"]) != 2 {
		t.Errorf("row = %v, want x=1 and x_2=2", row)
	}
}

func TestQueryEmptyResultset(t *testing.T) {
	p := newTestPool(t, nil)

	res := mustQuery(t, p, "SELECT id, name FROM users WHERE 1 = 0")

	if res.Columns == nil {
		t.Error("Columns is nil, want an empty-but-present list of column names")
	}
	if len(res.Columns) != 2 {
		t.Errorf("columns = %v, want the two selected names", res.Columns)
	}
	if res.Rows == nil {
		t.Error("Rows is nil, want an empty slice so it marshals as []")
	}
	if len(res.Rows) != 0 {
		t.Errorf("got %d rows, want 0", len(res.Rows))
	}
	if res.Truncated {
		t.Error("Truncated = true on an empty resultset")
	}
}

func TestQueryTruncation(t *testing.T) {
	// One connection, so "was the connection reused" is answerable.
	p := newTestPool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = 1
		cfg.Limits.MaxResponseBytes = 50 << 10
	})

	before := connectionID(t, p)

	res := mustQuery(t, p, "SELECT id, filler FROM big")
	if !res.Truncated {
		t.Fatal("Truncated = false, want true for a resultset over the byte cap")
	}
	if res.Note == "" {
		t.Error("Note is empty, want an explanation of the truncation")
	}
	if !strings.Contains(res.Note, "truncated") {
		t.Errorf("Note = %q, want it to mention truncation", res.Note)
	}
	if len(res.Rows) == 0 {
		t.Error("got 0 rows, want the rows read before the cap was hit")
	}
	if len(res.Columns) != 2 {
		t.Errorf("columns = %v, want the two selected names", res.Columns)
	}

	// Aborting mid-stream leaves unread packets on the wire, so that connection
	// must be dropped rather than handed to the next caller. Checked on the
	// pool itself: the Ping in acquire would otherwise hide a pooled-but-broken
	// connection by quietly replacing it.
	if idle := len(p.idle); idle != 0 {
		t.Errorf("%d connection(s) back in the pool after a truncated read, want 0", idle)
	}

	after := connectionID(t, p)
	if after == before {
		t.Errorf("connection %v was reused after a truncated read, want a fresh one", after)
	}

	// The pool still works afterwards.
	if res := mustQuery(t, p, "SELECT COUNT(*) AS n FROM users"); asNumber(t, res.Rows[0]["n"]) != 3 {
		t.Errorf("follow-up query returned %v, want 3", res.Rows[0]["n"])
	}
}

// The heart of the pool: a statement that changes session state must never
// leave that state visible to the next query.
func TestSessionStateIsolation(t *testing.T) {
	const timeout = 7
	p := newTestPool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = 1
		cfg.Limits.TimeoutSeconds = timeout
	})

	checkFences := func(t *testing.T, when string) {
		t.Helper()
		res := mustQuery(t, p, "SELECT @@session.tx_read_only AS ro, @@session.max_statement_time AS mst")
		if len(res.Rows) != 1 {
			t.Fatalf("%s: got %d rows, want 1", when, len(res.Rows))
		}
		if ro := asNumber(t, res.Rows[0]["ro"]); ro != 1 {
			t.Errorf("%s: tx_read_only = %v, want 1", when, ro)
		}
		if mst := asNumber(t, res.Rows[0]["mst"]); mst != timeout {
			t.Errorf("%s: max_statement_time = %v, want %d", when, mst, timeout)
		}
	}

	checkFences(t, "on a fresh connection")

	// Plain reads must keep reusing the same connection; the isolation fix
	// would be worthless if it threw away every connection.
	first := connectionID(t, p)
	if second := connectionID(t, p); second != first {
		t.Errorf("connection %v then %v for two plain reads, want the same one reused", first, second)
	}
	if idle := len(p.idle); idle != 1 {
		t.Errorf("%d connection(s) in the pool after a plain read, want 1", idle)
	}

	// Now poison the session through the same entry point a caller would use.
	poison := []string{
		"SET SESSION TRANSACTION READ WRITE",
		"SET SESSION max_statement_time=0",
	}
	for _, sql := range poison {
		res, err := query(t, p, sql)
		if err != nil {
			t.Fatalf("Query(%q): %v", sql, err)
		}
		if len(res.Rows) != 0 || len(res.Columns) != 0 {
			t.Errorf("Query(%q) returned %d columns and %d rows, want an empty result",
				sql, len(res.Columns), len(res.Rows))
		}
	}

	// A statement answered with an OK packet may have changed session state, so
	// its connection is discarded instead of pooled.
	if idle := len(p.idle); idle != 0 {
		t.Errorf("%d connection(s) back in the pool after SET statements, want 0", idle)
	}
	if after := connectionID(t, p); after == first {
		t.Errorf("connection %v was reused after SET statements, want a fresh one", after)
	}
	checkFences(t, "after SET statements")
}

func TestQueryTimeoutKills(t *testing.T) {
	// The server-side max_statement_time fence and the client-side deadline
	// both fire at the configured timeout, so with equal values it is a
	// coin flip which one wins. Keeping the server fence well above the
	// context deadline makes this test about the client-side KILL.
	p := newTestPool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = 1
		cfg.Limits.TimeoutSeconds = 20
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	res, err := p.Query(ctx, "SELECT SLEEP(10)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Query succeeded after %v, want a timeout error (result %+v)", elapsed, res)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the error", res)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want it to mention the timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("query took %v, want it killed shortly after the 1s deadline", elapsed)
	}

	// The statement must be gone from the server, not just abandoned by the
	// client. mcp_readonly only sees its own threads, which is enough here.
	time.Sleep(200 * time.Millisecond)
	res = mustQuery(t, p,
		"SELECT COUNT(*) AS n FROM information_schema.processlist WHERE command = 'Query' AND time > 0")
	if orphans := asNumber(t, res.Rows[0]["n"]); orphans != 0 {
		t.Errorf("%v query(ies) still running after the kill, want 0", orphans)
	}

	// The pool recovers.
	if res := mustQuery(t, p, "SELECT 1"); len(res.Rows) != 1 {
		t.Errorf("follow-up query returned %d rows, want 1", len(res.Rows))
	}
}

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	const (
		connections = 2
		queries     = 4
		sleep       = 0.5
	)
	p := newTestPool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = connections
		cfg.Limits.TimeoutSeconds = 15
	})

	// Dial both connections up front so the wall-clock measurement is about
	// queueing, not about connection setup.
	warm := mustQuery(t, p, "SELECT 1")
	if len(warm.Rows) != 1 {
		t.Fatalf("warm-up query returned %d rows, want 1", len(warm.Rows))
	}

	var wg sync.WaitGroup
	errs := make([]error, queries)
	rows := make([]int, queries)

	start := time.Now()
	for i := range queries {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := p.Query(ctx, fmt.Sprintf("SELECT SLEEP(%v)", sleep))
			errs[i] = err
			if res != nil {
				rows[i] = len(res.Rows)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i := range queries {
		if errs[i] != nil {
			t.Errorf("query %d: %v", i, errs[i])
		} else if rows[i] != 1 {
			t.Errorf("query %d returned %d rows, want 1", i, rows[i])
		}
	}

	// Four half-second queries through two slots take two waves. Only a lower
	// bound is asserted; dialling and scheduling add unpredictable time on top.
	wantAtLeast := time.Duration(float64(queries)/connections*sleep*0.9*1000) * time.Millisecond
	if elapsed < wantAtLeast {
		t.Errorf("%d queries through %d connections took %v, want at least %v — the semaphore did not queue them",
			queries, connections, elapsed, wantAtLeast)
	}
}

func TestOKPacketStatements(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) { cfg.Limits.MaxConnections = 1 })

	tests := []string{
		"SET SESSION max_statement_time=5",
		"SET @mcp_test_var = 1",
		"DO 1",
		"DO SLEEP(0)",
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			res, err := query(t, p, sql)
			if err != nil {
				t.Fatalf("Query(%q): %v", sql, err)
			}
			if len(res.Columns) != 0 {
				t.Errorf("columns = %v, want none", res.Columns)
			}
			if len(res.Rows) != 0 {
				t.Errorf("rows = %v, want none", res.Rows)
			}
			if res.Truncated || res.Note != "" {
				t.Errorf("Truncated = %v, Note = %q, want false and empty", res.Truncated, res.Note)
			}
		})
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	p := newTestPool(t, nil)

	// Two fences, either of which is enough: the read-only grant and the
	// read-only session transaction.
	_, err := query(t, p, "INSERT INTO users (name) VALUES ('should not happen')")
	if err == nil {
		t.Fatal("INSERT succeeded, want it rejected")
	}

	// The connection was dropped, but the pool keeps serving reads.
	if res := mustQuery(t, p, "SELECT COUNT(*) AS n FROM users"); asNumber(t, res.Rows[0]["n"]) != 3 {
		t.Errorf("users count = %v, want 3", res.Rows[0]["n"])
	}
}

func TestFullAccessSessionSetup(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) { cfg.Mode = modeFullAccess })

	res := mustQuery(t, p, "SELECT @@session.tx_read_only AS ro")
	if ro := asNumber(t, res.Rows[0]["ro"]); ro != 0 {
		t.Errorf("tx_read_only = %v, want 0 in full_access mode", ro)
	}
}

// full_access drops the app-level guard, so the grants become the boundary:
// the read-only user's INSERT must fail with a privilege error, not the
// read-only-transaction error.
func TestFullAccessWriteDeniedByGrants(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) { cfg.Mode = modeFullAccess })

	_, err := query(t, p, "INSERT INTO users (name) VALUES ('should not happen')")
	if err == nil {
		t.Fatal("INSERT as the read-only user succeeded, want a privilege error")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error = %q, want a privilege (command denied) error", err)
	}
}

// newWriteTestPool builds a full_access pool connecting as the seeded
// full-access user, skipping if that user is missing.
func newWriteTestPool(t *testing.T) *Pool {
	t.Helper()
	p := newTestPool(t, func(cfg *Config) {
		cfg.Mode = modeFullAccess
		cfg.Database.Username = envOr("MYSQL_TEST_WRITE_USER", "mcp_write")
		cfg.Database.Password = envOr("MYSQL_TEST_WRITE_PASSWORD", "devpassword")
	})
	if _, err := query(t, p, "SELECT 1"); err != nil {
		t.Skipf("full-access user unavailable — re-seed with seed/seed.sql: %v", err)
	}
	return p
}

func TestFullAccessWriteRoundTrip(t *testing.T) {
	p := newWriteTestPool(t)

	mustQuery(t, p, "DROP TABLE IF EXISTS mcp_write_test")
	mustQuery(t, p, "CREATE TABLE mcp_write_test (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(20))")
	t.Cleanup(func() { _, _ = query(t, p, "DROP TABLE IF EXISTS mcp_write_test") })

	ins := mustQuery(t, p, "INSERT INTO mcp_write_test (v) VALUES ('a'), ('b')")
	if ins.AffectedRows != uint64(2) {
		t.Errorf("INSERT affected_rows = %v, want 2", ins.AffectedRows)
	}
	if ins.LastInsertID == nil {
		t.Error("INSERT last_insert_id missing, want the new id")
	}

	upd := mustQuery(t, p, "UPDATE mcp_write_test SET v = 'c' WHERE v = 'a'")
	if upd.AffectedRows != uint64(1) {
		t.Errorf("UPDATE affected_rows = %v, want 1", upd.AffectedRows)
	}

	// A write that matches nothing still reports affected_rows: 0 — the field
	// must be present, not omitted.
	noop := mustQuery(t, p, "UPDATE mcp_write_test SET v = 'z' WHERE v = 'nope'")
	if noop.AffectedRows != uint64(0) {
		t.Errorf("no-op UPDATE affected_rows = %v, want 0", noop.AffectedRows)
	}

	// Every OK-packet statement still discards its connection (state safety
	// beats reuse), and reads keep flowing afterwards without write metadata.
	if idle := len(p.idle); idle != 0 {
		t.Errorf("%d connection(s) pooled after a write, want 0", idle)
	}
	sel := mustQuery(t, p, "SELECT COUNT(*) AS n FROM mcp_write_test")
	if asNumber(t, sel.Rows[0]["n"]) != 2 {
		t.Errorf("count = %v, want 2", sel.Rows[0]["n"])
	}
	if sel.AffectedRows != nil || sel.LastInsertID != nil {
		t.Errorf("SELECT reported affected_rows = %v, last_insert_id = %v, want both absent",
			sel.AffectedRows, sel.LastInsertID)
	}
}

func TestQueryError(t *testing.T) {
	p := newTestPool(t, nil)

	_, err := query(t, p, "SELECT * FROM table_that_does_not_exist")
	if err == nil {
		t.Fatal("query on a missing table succeeded, want an error")
	}

	if res := mustQuery(t, p, "SELECT 1"); len(res.Rows) != 1 {
		t.Errorf("follow-up query returned %d rows, want 1", len(res.Rows))
	}
}

func maskerForTest(t *testing.T, mask, except []string) *Masker {
	t.Helper()
	m, err := NewMasker(&MaskingConfig{Mask: mask, Except: except})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	return m
}

func TestQueryMasking(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) {
		cfg.masker = maskerForTest(t, []string{"phone", "email", "avatar"}, nil)
	})

	res := mustQuery(t, p, "SELECT id, name, email, phone, avatar FROM users ORDER BY id")

	first := res.Rows[0]
	if asNumber(t, first["id"]) != 1 {
		t.Errorf("id = %v, want 1", first["id"])
	}
	// Unlisted columns keep their real values.
	if first["name"] != "Andi Wijaya" {
		t.Errorf("name = %#v, want %q", first["name"], "Andi Wijaya")
	}
	if first["email"] != maskedValue {
		t.Errorf("email = %#v, want %q", first["email"], maskedValue)
	}
	if first["phone"] != maskedValue {
		t.Errorf("phone = %#v, want %q", first["phone"], maskedValue)
	}
	// A masked BLOB gets the policy placeholder, not the binary one.
	if first["avatar"] != maskedValue {
		t.Errorf("avatar = %#v, want %q", first["avatar"], maskedValue)
	}

	// NULL stays an explicit nil even in a masked column: whether a value
	// exists is not PII.
	second := res.Rows[1]
	if v, present := second["phone"]; !present || v != nil {
		t.Errorf("phone of row 2 = %#v (present %v), want an explicit nil", v, present)
	}
	if v, present := second["avatar"]; !present || v != nil {
		t.Errorf("avatar of row 2 = %#v (present %v), want an explicit nil", v, present)
	}

	wantMasked := []string{"email", "phone", "avatar"}
	if len(res.MaskedColumns) != len(wantMasked) {
		t.Fatalf("MaskedColumns = %v, want %v", res.MaskedColumns, wantMasked)
	}
	for i, want := range wantMasked {
		if res.MaskedColumns[i] != want {
			t.Errorf("MaskedColumns[%d] = %q, want %q", i, res.MaskedColumns[i], want)
		}
	}
	if !strings.Contains(res.Note, "PII policy") {
		t.Errorf("Note = %q, want it to explain the PII policy", res.Note)
	}

	// An alias cannot dodge masking; the reported label is the alias.
	res = mustQuery(t, p, "SELECT phone AS mobile FROM users WHERE id = 1")
	if res.Rows[0]["mobile"] != maskedValue {
		t.Errorf("mobile = %#v, want %q", res.Rows[0]["mobile"], maskedValue)
	}
	if len(res.MaskedColumns) != 1 || res.MaskedColumns[0] != "mobile" {
		t.Errorf("MaskedColumns = %v, want [mobile]", res.MaskedColumns)
	}
}

// wireOrgName reports the origin name the server sends for the first column,
// dialing directly so the test sees exactly the metadata Query sees.
func wireOrgName(t *testing.T, p *Pool, sql string) string {
	t.Helper()
	conn, err := client.Connect(p.addr, p.cfg.Database.Username,
		p.cfg.Database.Password, p.cfg.Database.DBName)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	res, err := conn.Execute(sql)
	if err != nil {
		if strings.Contains(err.Error(), "user_contacts") {
			t.Skipf("user_contacts view missing — re-seed with seed/seed.sql: %v", err)
		}
		t.Fatalf("Execute(%q): %v", sql, err)
	}
	if len(res.Fields) == 0 {
		t.Fatalf("Execute(%q) returned no fields", sql)
	}
	return string(res.Fields[0].OrgName)
}

// The views gap: the server does not read view definitions, so a plain column
// selected from a view takes the wire path and the outcome follows whatever
// origin the server reports through the view. That differs per server (MariaDB
// severs the origin through a renaming view, MySQL variants may not — spike
// 2026-08-14), so expectations are derived from the wire rather than
// hard-coded. The "contact" rule is the documented mitigation: name the view's
// own column and masking holds regardless of what the wire says about origins.
func TestQueryMaskingViewFollowsWireOrigin(t *testing.T) {
	const sql = "SELECT contact FROM user_contacts WHERE id = 1"
	for _, rule := range []string{"phone", "contact"} {
		t.Run(rule, func(t *testing.T) {
			p := newTestPool(t, func(cfg *Config) {
				cfg.masker = maskerForTest(t, []string{rule}, nil)
			})
			orgName := wireOrgName(t, p, sql)
			wantMasked := p.masker.Masked(nil, []byte(orgName))

			res := mustQuery(t, p, sql)
			got := res.Rows[0]["contact"]
			if wantMasked && got != maskedValue {
				t.Errorf("contact = %#v, want %q (wire org_name %q matches rule %q)",
					got, maskedValue, orgName, rule)
			}
			if !wantMasked && got != "081234567890" {
				t.Errorf("contact = %#v, want the raw value (wire org_name %q does not match rule %q)",
					got, orgName, rule)
			}
		})
	}
}

func TestQueryMaskingNoteMergesWithTruncation(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = 1
		cfg.Limits.MaxResponseBytes = 10 << 10
		cfg.masker = maskerForTest(t, []string{"filler"}, nil)
	})

	res := mustQuery(t, p, "SELECT id, filler FROM big")
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	for _, want := range []string{"PII policy", "truncated"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note = %q, want it to mention %q", res.Note, want)
		}
	}
	if len(res.Rows) == 0 || res.Rows[0]["filler"] != maskedValue {
		t.Errorf("filler = %#v, want %q", res.Rows[0]["filler"], maskedValue)
	}
}

// Masking enforcement end to end: the pen-test bypass queries must mask or
// refuse against a real database, not just in the planner unit tests.
func TestQueryStrictMasking(t *testing.T) {
	newStrictPool := func(t *testing.T) *Pool {
		return newTestPool(t, func(cfg *Config) {
			cfg.masker = maskerForTest(t, []string{"phone", "email"}, nil)
		})
	}

	t.Run("derived table alias no longer leaks (F2)", func(t *testing.T) {
		p := newStrictPool(t)
		res := mustQuery(t, p, "SELECT x FROM (SELECT phone AS x FROM users WHERE id = 1) t")
		if res.Rows[0]["x"] != maskedValue {
			t.Errorf("x = %#v, want %q", res.Rows[0]["x"], maskedValue)
		}
		if len(res.MaskedColumns) != 1 || res.MaskedColumns[0] != "x" {
			t.Errorf("MaskedColumns = %v, want [x]", res.MaskedColumns)
		}
	})

	t.Run("union no longer leaks (F1)", func(t *testing.T) {
		p := newStrictPool(t)
		res := mustQuery(t, p, "SELECT phone FROM users UNION ALL SELECT phone FROM users")
		masked := 0
		for _, row := range res.Rows {
			switch v := row["phone"]; v {
			case nil: // a NULL phone stays null
			case maskedValue:
				masked++
			default:
				t.Errorf("phone = %#v, want %q or null", v, maskedValue)
			}
		}
		if masked == 0 {
			t.Error("no phone values were masked in the union result")
		}
	})

	t.Run("group_concat no longer dumps (F5)", func(t *testing.T) {
		p := newStrictPool(t)
		res := mustQuery(t, p, "SELECT GROUP_CONCAT(phone) AS dump FROM users")
		if res.Rows[0]["dump"] != maskedValue {
			t.Errorf("dump = %#v, want %q", res.Rows[0]["dump"], maskedValue)
		}
	})

	t.Run("count is a number, not masked", func(t *testing.T) {
		p := newStrictPool(t)
		res := mustQuery(t, p, "SELECT COUNT(phone) AS n FROM users")
		if asNumber(t, res.Rows[0]["n"]) != 2 {
			t.Errorf("COUNT(phone) = %v, want 2", res.Rows[0]["n"])
		}
		if len(res.MaskedColumns) != 0 {
			t.Errorf("MaskedColumns = %v, want none for a count", res.MaskedColumns)
		}
	})

	t.Run("simple star still runs via the wire path", func(t *testing.T) {
		p := newStrictPool(t)
		res := mustQuery(t, p, "SELECT * FROM users ORDER BY id")
		if res.Rows[0]["phone"] != maskedValue {
			t.Errorf("phone = %#v, want %q", res.Rows[0]["phone"], maskedValue)
		}
		if res.Rows[0]["name"] != "Andi Wijaya" {
			t.Errorf("name = %#v, want the real value (not a masking rule)", res.Rows[0]["name"])
		}
	})

	t.Run("unverifiable query is refused before running", func(t *testing.T) {
		p := newStrictPool(t)
		_, err := query(t, p, "SELECT * FROM (SELECT phone FROM users) t")
		if err == nil {
			t.Fatal("query succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "SELECT *") {
			t.Errorf("error = %q, want it to name the SELECT * problem", err)
		}
		// The pool is untouched by a refused query and still serves reads.
		if res := mustQuery(t, p, "SELECT COUNT(*) AS n FROM users"); asNumber(t, res.Rows[0]["n"]) != 3 {
			t.Errorf("follow-up count = %v, want 3", res.Rows[0]["n"])
		}
	})

	t.Run("unparseable query is refused", func(t *testing.T) {
		p := newStrictPool(t)
		if _, err := query(t, p, "SELECT * FRM users"); err == nil {
			t.Fatal("garbage query succeeded, want a refusal")
		}
	})
}

func TestPoolDialFailure(t *testing.T) {
	p := newTestPool(t, func(cfg *Config) { cfg.Database.Password = "definitely-not-the-password" })

	_, err := query(t, p, "SELECT 1")
	if err == nil {
		t.Fatal("query with a bad password succeeded, want an error")
	}

	// A failed dial must hand the semaphore slot back, or the pool wedges after
	// MaxConnections failures.
	if len(p.sem) != 0 {
		t.Errorf("%d semaphore slot(s) still held after a failed dial, want 0", len(p.sem))
	}
}
