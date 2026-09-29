package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScrubCredentials(t *testing.T) {
	const M = maskedValue
	const hash = "*3FB4FB951367E478F1065E9F0B1DF24CC91ED7B2"

	tests := []struct {
		name        string
		in          string
		want        string
		wantSecrets []string
	}{
		// --- IDENTIFIED BY PASSWORD: MariaDB's SHOW GRANTS and SHOW CREATE USER ---
		{
			name:        "MariaDB SHOW GRANTS hash",
			in:          "GRANT USAGE ON *.* TO `mcp_agent`@`143.198.222.138` IDENTIFIED BY PASSWORD '" + hash + "'",
			want:        "GRANT USAGE ON *.* TO `mcp_agent`@`143.198.222.138` IDENTIFIED BY PASSWORD " + M,
			wantSecrets: []string{"'" + hash + "'"},
		},
		{
			name:        "MariaDB SHOW CREATE USER hash, then more clauses",
			in:          "CREATE USER `u`@`%` IDENTIFIED BY PASSWORD '" + hash + "' REQUIRE SSL",
			want:        "CREATE USER `u`@`%` IDENTIFIED BY PASSWORD " + M + " REQUIRE SSL",
			wantSecrets: []string{"'" + hash + "'"},
		},
		{
			name:        "grant option after the clause survives",
			in:          "GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost` IDENTIFIED BY PASSWORD '" + hash + "' WITH GRANT OPTION",
			want:        "GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost` IDENTIFIED BY PASSWORD " + M + " WITH GRANT OPTION",
			wantSecrets: []string{"'" + hash + "'"},
		},

		// --- IDENTIFIED BY: a plain password in query text ---
		{
			name:        "plain password",
			in:          "CREATE USER 'bob'@'%' IDENTIFIED BY 's3cret'",
			want:        "CREATE USER 'bob'@'%' IDENTIFIED BY " + M,
			wantSecrets: []string{"'s3cret'"},
		},
		{
			name:        "double-quoted password",
			in:          `ALTER USER bob IDENTIFIED BY "s3cret"`,
			want:        "ALTER USER bob IDENTIFIED BY " + M,
			wantSecrets: []string{`"s3cret"`},
		},
		{
			name:        "no space before the quote",
			in:          "ALTER USER bob IDENTIFIED BY's3cret'",
			want:        "ALTER USER bob IDENTIFIED BY" + M,
			wantSecrets: []string{"'s3cret'"},
		},
		{
			name:        "backslash-escaped quote inside",
			in:          `ALTER USER bob IDENTIFIED BY 'it\'s a secret' ACCOUNT LOCK`,
			want:        "ALTER USER bob IDENTIFIED BY " + M + " ACCOUNT LOCK",
			wantSecrets: []string{`'it\'s a secret'`},
		},
		{
			name:        "doubled quote inside",
			in:          "ALTER USER bob IDENTIFIED BY 'it''s a secret' ACCOUNT LOCK",
			want:        "ALTER USER bob IDENTIFIED BY " + M + " ACCOUNT LOCK",
			wantSecrets: []string{"'it''s a secret'"},
		},
		{
			name:        "escaped backslash just before the closing quote",
			in:          `ALTER USER bob IDENTIFIED BY 'ends in \\' ACCOUNT LOCK`,
			want:        "ALTER USER bob IDENTIFIED BY " + M + " ACCOUNT LOCK",
			wantSecrets: []string{`'ends in \\'`},
		},
		{
			name:        "REPLACE carries the current password",
			in:          "ALTER USER USER() IDENTIFIED BY 'new' REPLACE 'old' RETAIN CURRENT PASSWORD",
			want:        "ALTER USER USER() IDENTIFIED BY " + M + " REPLACE " + M + " RETAIN CURRENT PASSWORD",
			wantSecrets: []string{"'new'", "'old'"},
		},
		{
			name:        "RANDOM PASSWORD with REPLACE",
			in:          "ALTER USER USER() IDENTIFIED BY RANDOM PASSWORD REPLACE 'old'",
			want:        "ALTER USER USER() IDENTIFIED BY RANDOM PASSWORD REPLACE " + M,
			wantSecrets: []string{"'old'"},
		},

		// --- IDENTIFIED VIA: MariaDB plugins, alone and chained with OR ---
		{
			name:        "VIA with USING",
			in:          "CREATE USER `u`@`%` IDENTIFIED VIA ed25519 USING 'ZIgUREUg5PVgQ6LskhXmO+eZLS0nC8be6HPjYWR4YJY'",
			want:        "CREATE USER `u`@`%` IDENTIFIED VIA ed25519 USING " + M,
			wantSecrets: []string{"'ZIgUREUg5PVgQ6LskhXmO+eZLS0nC8be6HPjYWR4YJY'"},
		},
		{
			name:        "VIA chain, second alternative has no secret",
			in:          "GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost` IDENTIFIED VIA mysql_native_password USING 'invalid' OR unix_socket WITH GRANT OPTION",
			want:        "GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost` IDENTIFIED VIA mysql_native_password USING " + M + " OR unix_socket WITH GRANT OPTION",
			wantSecrets: []string{"'invalid'"},
		},
		{
			name:        "VIA chain, first alternative has no secret",
			in:          "CREATE USER u IDENTIFIED VIA unix_socket OR ed25519 USING PASSWORD('s3cret')",
			want:        "CREATE USER u IDENTIFIED VIA unix_socket OR ed25519 USING PASSWORD(" + M + ")",
			wantSecrets: []string{"'s3cret'"},
		},
		{
			name:        "VIA chain, every alternative has a secret",
			in:          "CREATE USER u IDENTIFIED VIA ed25519 USING PASSWORD('one') OR mysql_native_password USING '" + hash + "'",
			want:        "CREATE USER u IDENTIFIED VIA ed25519 USING PASSWORD(" + M + ") OR mysql_native_password USING " + M,
			wantSecrets: []string{"'one'", "'" + hash + "'"},
		},

		// --- IDENTIFIED WITH: MySQL 8 ---
		{
			name:        "WITH quoted plugin AS hash, plugin stays readable",
			in:          "CREATE USER `u`@`%` IDENTIFIED WITH 'caching_sha2_password' AS '$A$005$X3rk5KHo\\'P3Bz]7Wqz4fRqxGyxQ2p5Vw1n' REQUIRE NONE",
			want:        "CREATE USER `u`@`%` IDENTIFIED WITH 'caching_sha2_password' AS " + M + " REQUIRE NONE",
			wantSecrets: []string{"'$A$005$X3rk5KHo\\'P3Bz]7Wqz4fRqxGyxQ2p5Vw1n'"},
		},
		{
			name:        "WITH bare plugin AS hex literal",
			in:          "CREATE USER `u`@`%` IDENTIFIED WITH caching_sha2_password AS 0x244124303035240A1B2C REQUIRE NONE",
			want:        "CREATE USER `u`@`%` IDENTIFIED WITH caching_sha2_password AS " + M + " REQUIRE NONE",
			wantSecrets: []string{"0x244124303035240A1B2C"},
		},
		{
			name:        "WITH backticked plugin AS x'' literal",
			in:          "CREATE USER u IDENTIFIED WITH `mysql_native_password` AS X'2A3346'",
			want:        "CREATE USER u IDENTIFIED WITH `mysql_native_password` AS " + M,
			wantSecrets: []string{"X'2A3346'"},
		},
		{
			name:        "WITH plugin BY plain password",
			in:          "CREATE USER 'bob'@'%' IDENTIFIED WITH caching_sha2_password BY 's3cret' PASSWORD EXPIRE",
			want:        "CREATE USER 'bob'@'%' IDENTIFIED WITH caching_sha2_password BY " + M + " PASSWORD EXPIRE",
			wantSecrets: []string{"'s3cret'"},
		},
		{
			name:        "two factors, two clauses",
			in:          "CREATE USER u IDENTIFIED WITH caching_sha2_password BY 'one' AND IDENTIFIED WITH authentication_ldap_sasl AS 'uid=u'",
			want:        "CREATE USER u IDENTIFIED WITH caching_sha2_password BY " + M + " AND IDENTIFIED WITH authentication_ldap_sasl AS " + M,
			wantSecrets: []string{"'one'", "'uid=u'"},
		},

		// --- shapes ---
		{
			name:        "two clauses in one cell",
			in:          "CREATE USER a IDENTIFIED BY 'one';\nCREATE USER b IDENTIFIED BY PASSWORD '" + hash + "';",
			want:        "CREATE USER a IDENTIFIED BY " + M + ";\nCREATE USER b IDENTIFIED BY PASSWORD " + M + ";",
			wantSecrets: []string{"'one'", "'" + hash + "'"},
		},
		{
			name:        "mixed case and spread-out whitespace",
			in:          "grant usage on *.* to x Identified\n\tbY   PassWord  '" + hash + "'",
			want:        "grant usage on *.* to x Identified\n\tbY   PassWord  " + M,
			wantSecrets: []string{"'" + hash + "'"},
		},
		{
			// A database error quotes the statement from the fault, then
			// closes its own quote: the secret ends at the first of the two.
			name:        "error echo keeps its closing quote",
			in:          "You have an error in your SQL syntax; check the manual near 'USR x IDENTIFIED BY 'pw'' at line 1",
			want:        "You have an error in your SQL syntax; check the manual near 'USR x IDENTIFIED BY " + M + "' at line 1",
			wantSecrets: []string{"'pw'"},
		},
		{
			name:        "value cut off before its closing quote",
			in:          "CREATE USER 'bob'@'%' IDENTIFIED BY 'a-long-passw",
			want:        "CREATE USER 'bob'@'%' IDENTIFIED BY " + M,
			wantSecrets: []string{"'a-long-passw"},
		},
		{
			// The cell stays valid JSON: the secret is a whole SQL string,
			// never half of a JSON escape.
			name:        "clause inside a JSON cell",
			in:          `{"sql":"CREATE USER bob IDENTIFIED BY 'p\"w'","by":"admin"}`,
			want:        `{"sql":"CREATE USER bob IDENTIFIED BY ` + M + `","by":"admin"}`,
			wantSecrets: []string{`'p\"w'`},
		},

		// --- untouched ---
		{name: "no clause", in: "GRANT SELECT ON `mcp_dev`.* TO `mcp_readonly`@`localhost`", want: "GRANT SELECT ON `mcp_dev`.* TO `mcp_readonly`@`localhost`"},
		{name: "empty", in: "", want: ""},
		{name: "prose with the word", in: "Two rows were identified as duplicates.", want: "Two rows were identified as duplicates."},
		{name: "prose ending on the word", in: "The caller was never identified", want: "The caller was never identified"},
		{name: "part of a longer word", in: "an unidentified BY 'x' row", want: "an unidentified BY 'x' row"},
		{name: "plugin with no secret", in: "CREATE USER `u`@`localhost` IDENTIFIED VIA unix_socket", want: "CREATE USER `u`@`localhost` IDENTIFIED VIA unix_socket"},
		{name: "random password alone", in: "CREATE USER u IDENTIFIED BY RANDOM PASSWORD", want: "CREATE USER u IDENTIFIED BY RANDOM PASSWORD"},
		{name: "already masked", in: "GRANT USAGE ON *.* TO `u`@`%` IDENTIFIED BY PASSWORD " + M, want: "GRANT USAGE ON *.* TO `u`@`%` IDENTIFIED BY PASSWORD " + M},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, secrets := scrubCredentials(tt.in)
			if got != tt.want {
				t.Errorf("scrubCredentials(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
			if !reflect.DeepEqual(secrets, tt.wantSecrets) {
				t.Errorf("secrets = %q, want %q", secrets, tt.wantSecrets)
			}
		})
	}
}

func TestScrubCredentialsKeepsJSONValid(t *testing.T) {
	in := `{"sql":"CREATE USER bob IDENTIFIED BY 'p\"w\\\\'","n":1}`
	if !json.Valid([]byte(in)) {
		t.Fatalf("test input %s is not JSON", in)
	}
	got, secrets := scrubCredentials(in)
	if secrets == nil || !json.Valid([]byte(got)) {
		t.Errorf("scrubCredentials(%s) = %s (secrets %q), want a scrubbed, still valid JSON document", in, got, secrets)
	}
}

// The log path's statement scrub adds SET PASSWORD to the clauses a result
// cell is checked for.
func TestScrubStatement(t *testing.T) {
	const M = maskedValue
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"PASSWORD() form", "SET PASSWORD = PASSWORD('s3cret')", "SET PASSWORD = PASSWORD(" + M + ")"},
		{"for a user", "SET PASSWORD FOR 'bob'@'%' = PASSWORD('s3cret')", "SET PASSWORD FOR 'bob'@'%' = PASSWORD(" + M + ")"},
		{"OLD_PASSWORD() form", "set password for bob = old_password('s3cret')", "set password for bob = old_password(" + M + ")"},
		{"plain string", "SET PASSWORD FOR bob@localhost='s3cret'", "SET PASSWORD FOR bob@localhost=" + M},
		{"hash string", "SET PASSWORD = '*3FB4FB951367E478F1065E9F0B1DF24CC91ED7B2'", "SET PASSWORD = " + M},
		{"REPLACE the current one", "SET PASSWORD = 'new' REPLACE 'old' RETAIN CURRENT PASSWORD", "SET PASSWORD = " + M + " REPLACE " + M + " RETAIN CURRENT PASSWORD"},
		{"TO RANDOM with REPLACE", "SET PASSWORD TO RANDOM REPLACE 'old'", "SET PASSWORD TO RANDOM REPLACE " + M},
		{"IDENTIFIED still covered", "ALTER USER bob IDENTIFIED BY 's3cret'", "ALTER USER bob IDENTIFIED BY " + M},
		{"a password column is not a clause", "SELECT password FROM users WHERE id = 1", "SELECT password FROM users WHERE id = 1"},
		{"TO RANDOM alone", "SET PASSWORD TO RANDOM", "SET PASSWORD TO RANDOM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := scrubStatement(tt.in); got != tt.want {
				t.Errorf("scrubStatement(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}

	// SET PASSWORD is a statement, never what a result cell holds; the cell
	// scrub leaves it be, as the brief for the row path asks.
	if got, secrets := scrubCredentials("SET PASSWORD = 's3cret'"); secrets != nil {
		t.Errorf("scrubCredentials scrubbed a SET PASSWORD: %q", got)
	}
}

// An error that quotes the statement from a fault after IDENTIFIED has no
// keyword left to recognise the secret by; the statement's own secrets are
// removed from it verbatim.
func TestScrubEcho(t *testing.T) {
	const M = maskedValue
	sql := "CREATE USER x IDENTIFIED VIA ed25519 USING PASSWORD('s3cret')"
	_, secrets := scrubStatement(sql)
	errText := `line 1 column 27 near "VIA ed25519 USING PASSWORD('s3cret')" `
	if got, want := scrubEcho(errText, secrets), `line 1 column 27 near "VIA ed25519 USING PASSWORD(`+M+`)" `; got != want {
		t.Errorf("scrubEcho = %q, want %q", got, want)
	}

	// Patterns still run, for an echo that does include the keyword.
	if got, want := scrubEcho("near 'IDENTIFIED BY 'pw'' at line 1", nil), "near 'IDENTIFIED BY "+M+"' at line 1"; got != want {
		t.Errorf("scrubEcho = %q, want %q", got, want)
	}

	// An empty password is not a secret, and '' is everywhere in an error.
	_, secrets = scrubStatement("ALTER USER bob IDENTIFIED BY ''")
	if got, want := scrubEcho("Operation ALTER USER failed for ''@'%'", secrets), "Operation ALTER USER failed for ''@'%'"; got != want {
		t.Errorf("scrubEcho = %q, want %q", got, want)
	}
}

func TestContainsFold(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want bool
	}{
		{"identified", true},
		{"x IDENTIFIED y", true},
		{"IdEnTiFiEd", true},
		{"identifie", false},
		{"", false},
		{"iDENTIFIE\x00", false},
	} {
		if got := containsFold(tt.s, "identified"); got != tt.want {
			t.Errorf("containsFold(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

// Every text cell of every result goes through the scrub, so one with no
// clause in it must cost no allocation: the pre-filter turns it away before
// a regexp runs, and a cell that merely says "identified" allocates nothing
// either when no clause follows.
func TestScrubCredentialsIdleCellAllocatesNothing(t *testing.T) {
	for _, cell := range []string{
		"Kos Melati A1, kamar kosong 2, dekat kampus; hubungi pengelola untuk survei lokasi.",
		"Two rows were identified as duplicates.",
	} {
		if n := testing.AllocsPerRun(100, func() { scrubCredentials(cell) }); n != 0 {
			t.Errorf("scrubCredentials(%q) allocates %v times, want 0", cell, n)
		}
	}
}
