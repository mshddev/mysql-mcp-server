package main

import (
	"strings"
	"testing"
)

// Value masking end to end: the shape detectors catch personal data in cells
// the column rules cannot name, sit strictly below those rules, and respect
// masking.except. Needs the seeded database like the other integration tests.
func TestQueryValueMasking(t *testing.T) {
	newValuePool := func(t *testing.T, mc *MaskingConfig) *Pool {
		return newTestPool(t, func(cfg *Config) {
			m, err := NewMasker(mc)
			if err != nil {
				t.Fatalf("NewMasker: %v", err)
			}
			cfg.masker = m
		})
	}
	both := &MaskingConfig{Mask: []string{"phone", "email"}, Values: []string{"email", "phone_id"}}

	t.Run("free text with a phone inside", func(t *testing.T) {
		p := newValuePool(t, both)
		res := mustQuery(t, p, "SELECT 'Call me at 0812-3456-7890 after 5' AS notes")
		if want := "Call me at " + maskedValue + " after 5"; res.Rows[0]["notes"] != want {
			t.Errorf("notes = %#v, want %q", res.Rows[0]["notes"], want)
		}
		if len(res.MaskedColumns) != 0 {
			t.Errorf("MaskedColumns = %v, want none (a value hit is not a column decision)", res.MaskedColumns)
		}
		if got := res.MaskedValues["notes"]; len(got) != 1 || got[0] != "phone_id" {
			t.Errorf("MaskedValues = %v, want notes: [phone_id]", res.MaskedValues)
		}
		if !strings.Contains(res.Note, "phone_id") || !strings.Contains(res.Note, "notes") {
			t.Errorf("Note = %q, want it to name the detector and the column", res.Note)
		}
	})

	t.Run("column rule wins and the scanner is not consulted", func(t *testing.T) {
		p := newValuePool(t, both)
		res := mustQuery(t, p, "SELECT id, email, phone FROM users WHERE id = 1")
		if res.Rows[0]["email"] != maskedValue || res.Rows[0]["phone"] != maskedValue {
			t.Errorf("row = %v, want email and phone fully masked", res.Rows[0])
		}
		if res.MaskedValues != nil {
			t.Errorf("MaskedValues = %v, want none: column-masked cells are never scanned", res.MaskedValues)
		}
		if strings.Count(res.Note, "PII policy") != 1 {
			t.Errorf("Note = %q, want exactly the column sentence", res.Note)
		}
	})

	t.Run("view that renames a column is caught by shape", func(t *testing.T) {
		// The documented gap: no rule names the view's column, so the column
		// layer passes it on every server; the phone shape still catches it.
		p := newValuePool(t, both)
		res := mustQuery(t, p, "SELECT contact FROM user_contacts WHERE id = 1")
		if res.Rows[0]["contact"] != maskedValue {
			t.Errorf("contact = %#v, want %q", res.Rows[0]["contact"], maskedValue)
		}
		if res.MaskedValues["contact"] == nil && len(res.MaskedColumns) == 0 {
			t.Errorf("neither layer reported contact: MaskedColumns=%v MaskedValues=%v", res.MaskedColumns, res.MaskedValues)
		}
	})

	t.Run("values alone still mask by shape", func(t *testing.T) {
		p := newValuePool(t, &MaskingConfig{Values: []string{"email"}})
		res := mustQuery(t, p, "SELECT email, phone FROM users WHERE id = 1")
		if res.Rows[0]["email"] != maskedValue {
			t.Errorf("email = %#v, want %q", res.Rows[0]["email"], maskedValue)
		}
		if res.Rows[0]["phone"] != "081234567890" {
			t.Errorf("phone = %#v, want the raw value (phone_id not enabled)", res.Rows[0]["phone"])
		}
	})

	t.Run("except shields a plain column from both layers", func(t *testing.T) {
		p := newValuePool(t, &MaskingConfig{
			Mask: []string{"email"}, Except: []string{"users.email"}, Values: []string{"email"},
		})
		res := mustQuery(t, p, "SELECT email FROM users WHERE id = 1")
		if res.Rows[0]["email"] != "andi@example.com" {
			t.Errorf("email = %#v, want the raw value (excepted)", res.Rows[0]["email"])
		}
		if res.MaskedValues != nil || res.Note != "" {
			t.Errorf("MaskedValues=%v Note=%q, want nothing reported", res.MaskedValues, res.Note)
		}
		// Through a derived table the plan carries the exemption by position.
		res = mustQuery(t, p, "SELECT e FROM (SELECT email AS e FROM users WHERE id = 1) t")
		if res.Rows[0]["e"] != "andi@example.com" {
			t.Errorf("e = %#v, want the raw value (excepted through a derived table)", res.Rows[0]["e"])
		}
		// An expression has no single origin, so the shield does not apply.
		res = mustQuery(t, p, "SELECT CONCAT(email, '') AS e FROM users WHERE id = 1")
		if res.Rows[0]["e"] != maskedValue {
			t.Errorf("e = %#v, want %q (expression over an excepted column is still scanned)", res.Rows[0]["e"], maskedValue)
		}
	})

	t.Run("null, numbers and binary are untouched", func(t *testing.T) {
		p := newValuePool(t, both)
		res := mustQuery(t, p, "SELECT id, name, avatar FROM users WHERE id IN (1, 2) ORDER BY id")
		if v := res.Rows[0]["avatar"]; v != "<binary, 6 bytes>" {
			t.Errorf("avatar = %#v, want the binary placeholder", v)
		}
		if v, present := res.Rows[1]["avatar"]; !present || v != nil {
			t.Errorf("avatar of row 2 = %#v, want an explicit nil", v)
		}
		if asNumber(t, res.Rows[0]["id"]) != 1 || res.Rows[0]["name"] != "Andi Wijaya" {
			t.Errorf("row = %v, want id and name untouched", res.Rows[0])
		}
		if res.MaskedValues != nil {
			t.Errorf("MaskedValues = %v, want none", res.MaskedValues)
		}
	})

	t.Run("mixed column masks only matching rows", func(t *testing.T) {
		p := newValuePool(t, both)
		res := mustQuery(t, p, "SELECT id, name AS login FROM users UNION ALL SELECT 99, 'x@y.io' ORDER BY id")
		if res.Rows[0]["login"] != "Andi Wijaya" {
			t.Errorf("login of row 1 = %#v, want the real name", res.Rows[0]["login"])
		}
		last := res.Rows[len(res.Rows)-1]
		if last["login"] != maskedValue {
			t.Errorf("login of the appended row = %#v, want %q", last["login"], maskedValue)
		}
	})

	t.Run("value masking is off without the key", func(t *testing.T) {
		p := newValuePool(t, &MaskingConfig{Mask: []string{"phone"}})
		res := mustQuery(t, p, "SELECT email FROM users WHERE id = 1")
		if res.Rows[0]["email"] != "andi@example.com" {
			t.Errorf("email = %#v, want the raw value", res.Rows[0]["email"])
		}
		if res.MaskedValues != nil {
			t.Errorf("MaskedValues = %v, want none", res.MaskedValues)
		}
	})

	t.Run("note merges all three parts", func(t *testing.T) {
		p := newTestPool(t, func(cfg *Config) {
			cfg.Limits.MaxResponseBytes = 4 << 10
			m, err := NewMasker(&MaskingConfig{Mask: []string{"phone"}, Values: []string{"email"}})
			if err != nil {
				t.Fatalf("NewMasker: %v", err)
			}
			cfg.masker = m
		})
		res := mustQuery(t, p, "SELECT u.phone, 'a@b.io' AS tag, b.filler FROM big b JOIN users u ON u.id = 1")
		if !res.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		for _, want := range []string{"values in phone", "text matching email", "truncated"} {
			if !strings.Contains(res.Note, want) {
				t.Errorf("Note = %q, want it to contain %q", res.Note, want)
			}
		}
	})
}

// Under full_access a copy of a PII column into a fresh table has a name the
// rules never heard of; the shape detectors are the only layer that sees it.
func TestQueryValueMaskingCatchesLaunderedCopy(t *testing.T) {
	p := newWriteTestPool(t)
	m, err := NewMasker(&MaskingConfig{Mask: []string{"phone"}, Values: []string{"phone_id"}})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	m.bestEffort = true
	p.masker = m

	const scratch = "laundered_copy"
	drop := func() { mustQuery(t, p, "DROP TABLE IF EXISTS "+scratch) }
	drop()
	mustQuery(t, p, "CREATE TABLE "+scratch+" AS SELECT id, phone AS p FROM users")
	t.Cleanup(drop)

	res := mustQuery(t, p, "SELECT p FROM "+scratch+" WHERE id = 1")
	if res.Rows[0]["p"] != maskedValue {
		t.Errorf("p = %#v, want %q", res.Rows[0]["p"], maskedValue)
	}
	if len(res.MaskedColumns) != 0 {
		t.Errorf("MaskedColumns = %v, want none: no rule names p", res.MaskedColumns)
	}
	if got := res.MaskedValues["p"]; len(got) != 1 || got[0] != "phone_id" {
		t.Errorf("MaskedValues = %v, want p: [phone_id]", res.MaskedValues)
	}
}
