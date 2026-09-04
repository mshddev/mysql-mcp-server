package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDetectors(t *testing.T) {
	t.Run("known names in catalogue order", func(t *testing.T) {
		// Config order must not decide run order: email always goes first so
		// a digit-heavy local part is masked whole, not sliced by phone_id.
		got, err := parseDetectors([]string{"phone_id", "Email"})
		if err != nil {
			t.Fatalf("parseDetectors: %v", err)
		}
		names := make([]string, len(got))
		for i, d := range got {
			names[i] = d.name
		}
		if want := []string{"email", "phone_id"}; !reflect.DeepEqual(names, want) {
			t.Errorf("detectors = %v, want %v", names, want)
		}
	})
	t.Run("unknown name is a config error", func(t *testing.T) {
		_, err := parseDetectors([]string{"email", "ssn"})
		if err == nil || !strings.Contains(err.Error(), `"ssn"`) || !strings.Contains(err.Error(), "phone_id") {
			t.Errorf("err = %v, want it to name the bad entry and list the known detectors", err)
		}
	})
	t.Run("duplicates collapse", func(t *testing.T) {
		got, err := parseDetectors([]string{"email", "email"})
		if err != nil || len(got) != 1 {
			t.Errorf("got %d detectors, err %v; want 1 and nil", len(got), err)
		}
	})
}

func TestScanValue(t *testing.T) {
	m, err := NewMasker(&MaskingConfig{Values: []string{"email", "phone_id"}})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	const M = maskedValue

	tests := []struct {
		name      string
		in        string
		want      string
		wantFired []string
	}{
		{name: "plain text passes", in: "Kos Melati A1, kamar kosong 2", want: "Kos Melati A1, kamar kosong 2"},
		{name: "empty passes", in: "", want: ""},

		// --- email ---
		{name: "bare email masks whole cell", in: "budi@example.com", want: M, wantFired: []string{"email"}},
		{name: "email inside text", in: "reply to budi.s+kos@mail.example.co.id today", want: "reply to " + M + " today", wantFired: []string{"email"}},
		{name: "two emails", in: "a@x.io, b@y.io", want: M + ", " + M, wantFired: []string{"email"}},
		{name: "email inside json", in: `{"email":"a@b.com","action":"login"}`, want: `{"email":"` + M + `","action":"login"}`, wantFired: []string{"email"}},
		{name: "at sign without a domain is not an email", in: "meet @ 5pm", want: "meet @ 5pm"},
		{name: "handle is not an email", in: "@mamikos", want: "@mamikos"},

		// --- Indonesian phone ---
		{name: "08 form", in: "081234567890", want: M, wantFired: []string{"phone_id"}},
		{name: "08 form inside text", in: "Call me at 0812-3456-7890 after 5", want: "Call me at " + M + " after 5", wantFired: []string{"phone_id"}},
		{name: "spaced groups", in: "hp 0812 3456 7890", want: "hp " + M, wantFired: []string{"phone_id"}},
		{name: "62 form", in: "628123456789", want: M, wantFired: []string{"phone_id"}},
		{name: "+62 form", in: "wa +62 812-3456-7890 ya", want: "wa " + M + " ya", wantFired: []string{"phone_id"}},
		{name: "shortest valid (10 digits)", in: "0812345678", want: M, wantFired: []string{"phone_id"}},
		{name: "longest valid (13 digits)", in: "0812345678901", want: M, wantFired: []string{"phone_id"}},
		{name: "too short is not a phone", in: "081234567", want: "081234567"},
		{name: "too long is not a phone", in: "08123456789012", want: "08123456789012"},
		{name: "landline prefix is not caught", in: "0274123456", want: "0274123456"},
		{name: "epoch millis are not a phone", in: "1725400000000", want: "1725400000000"},
		{name: "glued to a longer number is not a phone", in: "9908123456789", want: "9908123456789"},
		{name: "digit run after it is not a phone", in: "08123456789012345", want: "08123456789012345"},
		{name: "non-digit boundary is enough", in: "INV-0812345678", want: "INV-" + M, wantFired: []string{"phone_id"}},
		// A greedy match must not swallow a following number and then give
		// up on the whole span (the leak the first version had).
		{name: "space then more digits after the phone", in: "081234567890 2026", want: M + " 2026", wantFired: []string{"phone_id"}},
		{name: "space then a price after the phone", in: "0812345678 1500000", want: M + " 1500000", wantFired: []string{"phone_id"}},
		{name: "two phones separated by a space", in: "081234567890 085612345678", want: M + " " + M, wantFired: []string{"phone_id"}},
		{name: "two phones separated by a slash", in: "081234567890/085612345678", want: M + "/" + M, wantFired: []string{"phone_id"}},
		{name: "decimal is not a phone", in: "0.812345678", want: "0.812345678"},
		{name: "separator after the country code only", in: "+62-812-3456-7890", want: M, wantFired: []string{"phone_id"}},

		// --- both ---
		{name: "email then phone", in: "andi@example.com / 081234567890", want: M + " / " + M, wantFired: []string{"email", "phone_id"}},
		{name: "digits in an email local part stay an email", in: "081234567890@example.com", want: M, wantFired: []string{"email"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fired := m.scanValue(tt.in)
			if got != tt.want {
				t.Errorf("scanValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !reflect.DeepEqual(fired, tt.wantFired) {
				t.Errorf("fired = %v, want %v", fired, tt.wantFired)
			}
		})
	}
}

func TestScanValueOnlyEnabledDetectors(t *testing.T) {
	m, err := NewMasker(&MaskingConfig{Values: []string{"email"}})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	if got, fired := m.scanValue("081234567890"); got != "081234567890" || fired != nil {
		t.Errorf("phone masked with only email enabled: %q, %v", got, fired)
	}
	var none *Masker
	if got, fired := none.scanValue("a@b.com"); got != "a@b.com" || fired != nil {
		t.Errorf("nil masker scanned a value: %q, %v", got, fired)
	}
	if none.scansValues() {
		t.Error("nil masker reports value scanning")
	}
}

func TestValueHitsReport(t *testing.T) {
	h := valueHits{}
	if h.report([]string{"a"}) != nil {
		t.Error("empty hits should report nil so the field is omitted")
	}
	h.add(1, []string{"phone_id"})
	h.add(1, []string{"email", "phone_id"})
	h.add(0, []string{"email"})
	got := h.report([]string{"notes", "payload"})
	want := map[string][]string{"notes": {"email"}, "payload": {"email", "phone_id"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %v, want %v", got, want)
	}
}

func TestValueMaskNote(t *testing.T) {
	got := valueMaskNote(map[string][]string{"payload": {"phone_id"}, "notes": {"email", "phone_id"}})
	want := `text matching email, phone_id patterns is "<masked>" inside notes, payload by server PII policy`
	if got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

func TestMaskerExcepted(t *testing.T) {
	m, err := NewMasker(&MaskingConfig{
		Mask:   []string{"*_email"},
		Except: []string{"merchants.support_email"},
	})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	tests := []struct {
		table, column string
		want          bool
	}{
		{"merchants", "support_email", true},
		{"MERCHANTS", "Support_Email", true},
		{"users", "support_email", false},
		{"merchants", "", false},
	}
	for _, tt := range tests {
		if got := m.Excepted([]byte(tt.table), []byte(tt.column)); got != tt.want {
			t.Errorf("Excepted(%q, %q) = %v, want %v", tt.table, tt.column, got, tt.want)
		}
	}
	var none *Masker
	if none.Excepted([]byte("merchants"), []byte("support_email")) {
		t.Error("nil masker excepted a column")
	}
}
