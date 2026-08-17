package main

import (
	"slices"
	"strings"
	"testing"
)

func TestPlanQuery(t *testing.T) {
	m := maskerForTest(t, []string{"phone", "*_phone", "email", "users.address"}, nil)

	tests := []struct {
		name     string
		sql      string
		wantErr  string  // non-empty => expect a refusal mentioning this
		wire     bool    // expect useWire (simple query, tag trusted)
		wantMask []bool  // expected per-column decision when traced
	}{
		// --- simple shapes take the wire path ---
		{name: "plain column", sql: "SELECT phone FROM users", wire: true},
		{name: "star on a base table", sql: "SELECT * FROM users", wire: true},
		{name: "aliased plain column", sql: "SELECT phone AS x FROM users", wire: true},
		{name: "base-table join", sql: "SELECT u.phone, o.total FROM users u JOIN orders o ON o.uid = u.id", wire: true},
		{name: "show", sql: "SHOW TABLES", wire: true},
		{name: "describe", sql: "DESCRIBE users", wire: true},
		{name: "explain", sql: "EXPLAIN SELECT phone FROM users", wire: true},

		// --- computed columns are traced ---
		{name: "concat of pii is masked", sql: "SELECT CONCAT(phone, 'x') AS c FROM users", wantMask: []bool{true}},
		{name: "group_concat of pii is masked (F5)", sql: "SELECT GROUP_CONCAT(phone) AS d FROM users", wantMask: []bool{true}},
		{name: "max of pii is masked", sql: "SELECT MAX(phone) AS m FROM users", wantMask: []bool{true}},
		{name: "plain count is a number", sql: "SELECT COUNT(phone) AS n FROM users", wantMask: []bool{false}},
		{name: "count star is a number", sql: "SELECT COUNT(*) AS n FROM users", wantMask: []bool{false}},
		{name: "mixed", sql: "SELECT id, CONCAT(phone) AS c FROM users", wantMask: []bool{false, true}},
		{name: "expression over non-pii passes", sql: "SELECT CONCAT(id, room_name) AS c FROM bookings", wantMask: []bool{false}},

		// --- the peer's derived-table / CTE / union bypasses ---
		{name: "derived + alias (F2)", sql: "SELECT x FROM (SELECT phone AS x FROM users) t", wantMask: []bool{true}},
		{name: "cte (F2)", sql: "WITH c AS (SELECT phone AS p FROM users) SELECT p FROM c", wantMask: []bool{true}},
		{name: "union same column (F1)", sql: "SELECT phone FROM users UNION ALL SELECT phone FROM users", wantMask: []bool{true}},
		{
			name:     "union smuggle (F1)",
			sql:      "SELECT id, name FROM users WHERE 1=0 UNION ALL SELECT id, phone FROM users",
			wantMask: []bool{false, true}, // the column labelled name secretly carries phone
		},
		{name: "scalar subquery of pii", sql: "SELECT (SELECT phone FROM users LIMIT 1) AS x FROM bookings", wantMask: []bool{true}},

		// --- tracing through a join inside a derived table ---
		{name: "qualified pii through derived join", sql: "SELECT c FROM (SELECT u.phone AS c FROM users u JOIN bookings b ON b.user_id = u.id) t", wantMask: []bool{true}},
		{name: "qualified non-pii through derived join", sql: "SELECT c FROM (SELECT b.price AS c FROM users u JOIN bookings b ON b.user_id = u.id) t", wantMask: []bool{false}},
		{name: "ambiguous column in derived join is hidden", sql: "SELECT c FROM (SELECT status AS c FROM users u JOIN bookings b ON b.user_id = u.id) t", wantMask: []bool{true}},

		// --- a nested star we can't enumerate: hide the column, don't refuse ---
		{name: "star inside a derived table hides the outer column", sql: "SELECT x FROM (SELECT * FROM users) t", wantMask: []bool{true}},

		// --- window functions: value comes from args, not the ordering ---
		{name: "row_number over pii ordering is not masked", sql: "SELECT ROW_NUMBER() OVER (ORDER BY phone) AS rn, id FROM users", wantMask: []bool{false, false}},
		{name: "lag of a pii column is masked", sql: "SELECT LAG(phone) OVER (ORDER BY id) AS l FROM users", wantMask: []bool{true}},
		{name: "first_value of pii is masked", sql: "SELECT FIRST_VALUE(phone) OVER (ORDER BY id) AS f FROM users", wantMask: []bool{true}},

		// --- refusals ---
		{name: "star over a derived table", sql: "SELECT * FROM (SELECT phone FROM users) t", wantErr: "SELECT *"},
		{name: "star in a union branch", sql: "SELECT * FROM users UNION SELECT * FROM users", wantErr: "SELECT *"},
		{name: "table form is refused (MySQL 8 SELECT *)", sql: "TABLE users", wantErr: "TABLE or VALUES"},
		{name: "values form is refused", sql: "VALUES ROW(1, 2)", wantErr: "TABLE or VALUES"},
		{name: "non-select", sql: "INSERT INTO users (name) VALUES ('x')", wantErr: "only SELECT"},
		{name: "unparseable", sql: "SELECT * FRM users", wantErr: "could not parse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := m.planQuery(tt.sql)
			checkPlan(t, tt.sql, plan, err, tt.wantErr, tt.wire, tt.wantMask)
		})
	}
}

// Best effort (full_access mode): statements strict masking refuses — writes,
// DDL, anything unparseable — pass with wire-tag masking, while parseable
// reads keep the full strict treatment.
func TestPlanQueryBestEffort(t *testing.T) {
	m := maskerForTest(t, []string{"phone", "*_phone", "email", "users.address"}, nil)
	m.bestEffort = true

	tests := []struct {
		name     string
		sql      string
		wantErr  string
		wire     bool
		wantMask []bool
	}{
		{name: "insert passes", sql: "INSERT INTO users (name) VALUES ('x')", wire: true},
		{name: "update passes", sql: "UPDATE users SET name = 'x' WHERE id = 1", wire: true},
		{name: "delete passes", sql: "DELETE FROM users WHERE id = 1", wire: true},
		{name: "ddl passes", sql: "CREATE TABLE scratch (id INT)", wire: true},
		{name: "drop passes", sql: "DROP TABLE scratch", wire: true},
		// Unclassifiable: may be dialect syntax the parser's grammar lacks.
		{name: "unparseable passes", sql: "SELECT * FRM users", wire: true},

		// Reads stay strict, refusals included.
		{name: "derived alias is still traced", sql: "SELECT x FROM (SELECT phone AS x FROM users) t", wantMask: []bool{true}},
		{name: "star over a derived table is still refused", sql: "SELECT * FROM (SELECT phone FROM users) t", wantErr: "SELECT *"},
		{name: "table form is still refused", sql: "TABLE users", wantErr: "TABLE or VALUES"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := m.planQuery(tt.sql)
			checkPlan(t, tt.sql, plan, err, tt.wantErr, tt.wire, tt.wantMask)
		})
	}
}

func checkPlan(t *testing.T, sql string, plan *queryPlan, err error, wantErr string, wire bool, wantMask []bool) {
	t.Helper()
	if wantErr != "" {
		if err == nil {
			t.Fatalf("planQuery(%q) succeeded, want a refusal mentioning %q", sql, wantErr)
		}
		if !strings.Contains(err.Error(), wantErr) {
			t.Errorf("refusal %q does not mention %q", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("planQuery(%q): %v", sql, err)
	}
	if wire {
		if !plan.useWire {
			t.Errorf("useWire = false, want true (mask=%v)", plan.mask)
		}
		return
	}
	if plan.useWire {
		t.Fatalf("useWire = true, want the query traced")
	}
	if !slices.Equal(plan.mask, wantMask) {
		t.Errorf("mask = %v, want %v", plan.mask, wantMask)
	}
}
