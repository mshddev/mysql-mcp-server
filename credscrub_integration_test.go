package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// leakedHash is what a credential looks like in SHOW GRANTS or SHOW CREATE
// USER: a mysql_native_password hash (* and 40 hex digits, MariaDB and old
// MySQL) or a caching_sha2_password one ($A$ and a round count, MySQL 8).
var leakedHash = regexp.MustCompile(`\*[0-9A-Fa-f]{40}|\$A\$[0-9]{3}\$`)

// The credential scrub end to end, against the seeded database. MariaDB
// prints the connected user's hash in both statements and MySQL 8 prints it
// in SHOW CREATE USER only, so what holds on every server is that no hash
// comes back, not that a <masked> does.
func TestQueryScrubsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		mc   *MaskingConfig
	}{
		// The scrub owes nothing to the masking section.
		{"masking off", nil},
		{"column rules on", &MaskingConfig{Mask: []string{"email"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, func(cfg *Config) {
				m, err := NewMasker(tc.mc)
				if err != nil {
					t.Fatalf("NewMasker: %v", err)
				}
				cfg.masker = m
			})
			for _, sql := range []string{"SHOW GRANTS", "SHOW CREATE USER CURRENT_USER()"} {
				res := mustQuery(t, p, sql)
				raw, err := json.Marshal(res)
				if err != nil {
					t.Fatalf("encode result: %v", err)
				}
				if leak := leakedHash.Find(raw); leak != nil {
					t.Errorf("%s returned a password hash (%s): %s", sql, leak, raw)
				}
				if len(res.Rows) == 0 {
					t.Errorf("%s returned no rows, want the grants still readable", sql)
				}
				// Where the scrub fired, the caller is told so.
				if strings.Contains(string(raw), maskedValue) {
					for col, names := range res.MaskedValues {
						if len(names) != 1 || names[0] != credentialDetector {
							t.Errorf("%s: masked_values[%q] = %v, want [%s]", sql, col, names, credentialDetector)
						}
					}
					if len(res.MaskedValues) == 0 || !strings.Contains(res.Note, "credentials inside") {
						t.Errorf("%s: masked_values = %v, note = %q, want the credential reported", sql, res.MaskedValues, res.Note)
					}
				}
			}
		})
	}

	// The same scrub on a cell that holds a clause on every server, with
	// the rest of the text left readable.
	t.Run("clause in a text cell", func(t *testing.T) {
		p := newTestPool(t, nil)
		res := mustQuery(t, p, "SELECT 'CREATE USER bob IDENTIFIED BY ''s3cret'' ACCOUNT LOCK' AS stmt")
		if want := "CREATE USER bob IDENTIFIED BY " + maskedValue + " ACCOUNT LOCK"; res.Rows[0]["stmt"] != want {
			t.Errorf("stmt = %#v, want %q", res.Rows[0]["stmt"], want)
		}
		if got := res.MaskedValues["stmt"]; len(got) != 1 || got[0] != credentialDetector {
			t.Errorf("MaskedValues = %v, want stmt: [%s]", res.MaskedValues, credentialDetector)
		}
		if len(res.MaskedColumns) != 0 {
			t.Errorf("MaskedColumns = %v, want none: the rest of the cell is real", res.MaskedColumns)
		}
	})

	// masking.except shields a column from the masking layers, not from
	// this: the scrub runs first and regardless. The excepted column is the
	// processlist's text of the running statement, which is this one,
	// comment included; the email left standing in it shows the except did
	// apply.
	t.Run("except does not switch it off", func(t *testing.T) {
		p := newTestPool(t, func(cfg *Config) {
			m, err := NewMasker(&MaskingConfig{
				Mask: []string{"email"}, Except: []string{"info"}, Values: []string{"email"},
			})
			if err != nil {
				t.Fatalf("NewMasker: %v", err)
			}
			cfg.masker = m
		})
		res := mustQuery(t, p, "SELECT INFO FROM information_schema.PROCESSLIST WHERE ID = CONNECTION_ID()"+
			" /* IDENTIFIED BY 's3cret' andi@example.com */")
		if len(res.Rows) != 1 {
			t.Fatalf("rows = %v, want this statement's own processlist row", res.Rows)
		}
		got, _ := res.Rows[0]["INFO"].(string)
		if !strings.Contains(got, "andi@example.com") {
			t.Fatalf("INFO = %q, want the email left alone: the column is excepted from masking", got)
		}
		if strings.Contains(got, "s3cret") || !strings.Contains(got, "IDENTIFIED BY "+maskedValue+" andi@") {
			t.Errorf("INFO = %q, want the password scrubbed all the same", got)
		}
		if names := res.MaskedValues["INFO"]; len(names) != 1 || names[0] != credentialDetector {
			t.Errorf("MaskedValues = %v, want INFO: [%s]", res.MaskedValues, credentialDetector)
		}
	})
}
