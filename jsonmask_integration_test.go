package main

import (
	"strings"
	"testing"
)

// JSON masking end to end: a JSON-valued cell reaches the walker through the
// same row callback as everything else, the key rules and the detectors both
// apply inside it, and the response reports the keys. Needs the seeded
// database like the other integration tests.
func TestQueryJSONMasking(t *testing.T) {
	newPool := func(t *testing.T, mc *MaskingConfig) *Pool {
		return newTestPool(t, func(cfg *Config) {
			m, err := NewMasker(mc)
			if err != nil {
				t.Fatalf("NewMasker: %v", err)
			}
			cfg.masker = m
		})
	}
	rules := &MaskingConfig{Mask: []string{"phone", "name", "email"}, Values: []string{"email", "phone_id"}}

	t.Run("literal json cell", func(t *testing.T) {
		p := newPool(t, rules)
		res := mustQuery(t, p, `SELECT '{"name":"Jane","phone":"081234567890","plan":2,"note":"cc a@b.com"}' AS props`)
		want := `{"name":"<masked>","note":"cc <masked>","phone":"<masked>","plan":2}`
		if res.Rows[0]["props"] != want {
			t.Errorf("props = %#v, want %q", res.Rows[0]["props"], want)
		}
		if len(res.MaskedColumns) != 0 {
			t.Errorf("MaskedColumns = %v, want none (a key hit is not a column decision)", res.MaskedColumns)
		}
		if got := res.MaskedJSONKeys["props"]; len(got) != 2 || got[0] != "name" || got[1] != "phone" {
			t.Errorf("MaskedJSONKeys = %v, want props: [name phone]", res.MaskedJSONKeys)
		}
		if got := res.MaskedValues["props"]; len(got) != 1 || got[0] != "email" {
			t.Errorf("MaskedValues = %v, want props: [email]", res.MaskedValues)
		}
		for _, w := range []string{"JSON keys name, phone", "props", "email"} {
			if !strings.Contains(res.Note, w) {
				t.Errorf("Note = %q, want it to mention %q", res.Note, w)
			}
		}
	})

	t.Run("seeded column through JSON_OBJECT", func(t *testing.T) {
		// filler is business data with no PII origin, so the column rules
		// pass the cell and only the walker can reach the key inside it.
		p := newPool(t, rules)
		res := mustQuery(t, p, "SELECT JSON_OBJECT('name', filler, 'id', id) AS doc FROM big WHERE id = 1")
		doc, _ := res.Rows[0]["doc"].(string)
		if !strings.Contains(doc, `"name": "<masked>"`) && !strings.Contains(doc, `"name":"<masked>"`) {
			t.Errorf("doc = %q, want the name key masked", doc)
		}
		// Which row id 1 holds depends on the engine: the seed's INSERT ...
		// SELECT has no ORDER BY, and MySQL and MariaDB emit the cross join
		// in different orders. Every filler starts with "row-", which is
		// what the leak check needs.
		if strings.Contains(doc, "row-") {
			t.Errorf("doc = %q, the filler leaked", doc)
		}
		if !strings.Contains(doc, `"id"`) {
			t.Errorf("doc = %q, the id key went missing", doc)
		}
		if got := res.MaskedJSONKeys["doc"]; len(got) != 1 || got[0] != "name" {
			t.Errorf("MaskedJSONKeys = %v, want doc: [name]", res.MaskedJSONKeys)
		}
		// Only keys matched here: no detector fired, so the value layer must
		// stay silent in both the field and the note.
		if res.MaskedValues != nil {
			t.Errorf("MaskedValues = %v, want nil when only keys matched", res.MaskedValues)
		}
		if strings.Contains(res.Note, "text matching") {
			t.Errorf("Note = %q, carries a value-mask sentence with no detector", res.Note)
		}
	})

	t.Run("no hit leaves the cell byte-identical", func(t *testing.T) {
		p := newPool(t, rules)
		const cell = `{"zeta": 1, "alpha": [true, null]}`
		res := mustQuery(t, p, "SELECT '"+cell+"' AS props")
		if res.Rows[0]["props"] != cell {
			t.Errorf("props = %#v, want %q untouched", res.Rows[0]["props"], cell)
		}
		if res.MaskedJSONKeys != nil || res.MaskedValues != nil || res.Note != "" {
			t.Errorf("reported hits on a cell with none: keys %v values %v note %q",
				res.MaskedJSONKeys, res.MaskedValues, res.Note)
		}
	})

	t.Run("excepted column is shielded from the walker", func(t *testing.T) {
		p := newPool(t, &MaskingConfig{Mask: []string{"name"}, Except: []string{"big.filler"}})
		res := mustQuery(t, p, "SELECT filler FROM big WHERE id = 1")
		// "row-" rather than "row-1-": see the JSON_OBJECT case above.
		if got, _ := res.Rows[0]["filler"].(string); !strings.HasPrefix(got, "row-") {
			t.Errorf("filler = %q, want the real value", got)
		}
	})

	t.Run("qualified rules only means no parse", func(t *testing.T) {
		p := newPool(t, &MaskingConfig{Mask: []string{"users.phone"}, Values: []string{"phone_id"}})
		const cell = `{"phone":"081234567890","z":1,"a":2}`
		res := mustQuery(t, p, "SELECT '"+cell+"' AS props")
		// The raw detector masked the number in place; the key order proves
		// the cell was never re-serialised.
		want := `{"phone":"<masked>","z":1,"a":2}`
		if res.Rows[0]["props"] != want {
			t.Errorf("props = %#v, want %q", res.Rows[0]["props"], want)
		}
		if res.MaskedJSONKeys != nil {
			t.Errorf("MaskedJSONKeys = %v, want none", res.MaskedJSONKeys)
		}
	})
}
