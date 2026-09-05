package main

import (
	"reflect"
	"strings"
	"testing"
)

func jsonMasker(t *testing.T, mc *MaskingConfig) *Masker {
	t.Helper()
	m, err := NewMasker(mc)
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}
	return m
}

func TestMaskJSON(t *testing.T) {
	m := jsonMasker(t, &MaskingConfig{
		Mask:   []string{"phone", "*_email", "name", "address", "users.nik"},
		Except: []string{"room_name", "meta"},
		Values: []string{"email", "phone_id"},
	})
	const M = `"` + maskedValue + `"`

	tests := []struct {
		name      string
		in        string
		want      string // empty means "unchanged, byte for byte"
		wantKeys  []string
		wantFired []string
	}{
		{
			name:     "flat object",
			in:       `{"id":7,"name":"Jane","phone":"0812","plan":2}`,
			want:     `{"id":7,"name":` + M + `,"phone":` + M + `,"plan":2}`,
			wantKeys: []string{"name", "phone"},
		},
		{
			name:     "nested activity log entry",
			in:       `{"attributes":{"name":"Jane","phone":"0812","room":3},"old":{"name":"Jan"}}`,
			want:     `{"attributes":{"name":` + M + `,"phone":` + M + `,"room":3},"old":{"name":` + M + `}}`,
			wantKeys: []string{"name", "phone"},
		},
		{
			name:     "null stays null",
			in:       `{"name":null,"phone":"0812"}`,
			want:     `{"name":null,"phone":` + M + `}`,
			wantKeys: []string{"phone"},
		},
		{
			name:     "matched key hides its whole subtree",
			in:       `{"address":{"street":"Jl. Melati 1","city":"Bandung"},"tags":["a"]}`,
			want:     `{"address":` + M + `,"tags":["a"]}`,
			wantKeys: []string{"address"},
		},
		{
			name:     "matched key hides an array value",
			in:       `[{"phone":["0812","0813"]},{"n":1}]`,
			want:     `[{"phone":` + M + `},{"n":1}]`,
			wantKeys: []string{"phone"},
		},
		{
			name:     "keys match case-insensitively, reported as written",
			in:       `{"Name":"Jane","PHONE":"0812"}`,
			want:     `{"Name":` + M + `,"PHONE":` + M + `}`,
			wantKeys: []string{"Name", "PHONE"},
		},
		{
			name:     "glob rule reaches keys",
			in:       `{"contact_email":"x","billing_email":"y","email_ok":true}`,
			want:     `{"billing_email":` + M + `,"contact_email":` + M + `,"email_ok":true}`,
			wantKeys: []string{"billing_email", "contact_email"},
		},
		{
			name: "except beats mask",
			in:   `{"room_name":"Melati A1","name":"Jane"}`,
			want: `{"name":` + M + `,"room_name":"Melati A1"}`,
			// room_name is excepted, and would not match "name" (an exact
			// rule) anyway; the point is it stays readable.
			wantKeys: []string{"name"},
		},
		{
			name: "excepted subtree shields the detectors too",
			in:   `{"meta":{"from":"a@b.com"},"note":"c@d.com"}`,
			want: `{"meta":{"from":"a@b.com"},"note":` + M + `}`,
			// The column-level rule: except trusts the whole thing.
			wantFired: []string{"email"},
		},
		{
			name: "qualified rules are column rules only",
			in:   `{"nik":"3273010101010001"}`,
		},
		{
			name:      "detectors run on leaf strings the keys left alone",
			in:        `{"note":"call 0812-3456-7890 or a@b.com","n":1}`,
			want:      `{"n":1,"note":"call ` + maskedValue + ` or ` + maskedValue + `"}`,
			wantFired: []string{"email", "phone_id"},
		},
		{
			name:     "big integer survives the round trip",
			in:       `{"id":9007199254740993,"phone":"0812","ratio":0.10}`,
			want:     `{"id":9007199254740993,"phone":` + M + `,"ratio":0.10}`,
			wantKeys: []string{"phone"},
		},
		{
			name: "no hit is byte-identical, keys not reordered",
			in:   `{"zeta":1, "alpha":  [true,null], "note":"plain"}`,
		},
		{
			name: "not json falls through",
			in:   "Jane phone: none",
		},
		{
			name: "broken json falls through",
			in:   `{"phone":"0812"`,
		},
		{
			name: "trailing content falls through rather than being dropped",
			in:   `{"phone":"0812"} and more`,
		},
		{
			name:     "leading whitespace is still json",
			in:       "  \n{\"phone\":\"0812\"}",
			want:     `{"phone":` + M + `}`,
			wantKeys: []string{"phone"},
		},
		{
			name: "a top-level string is not walked",
			in:   `"phone"`,
		},
		{
			name:     "unicode and slashes are not escaped",
			in:       `{"name":"Jane","note":"Jl. Melati 1/2 – kamar <A>"}`,
			want:     `{"name":` + M + `,"note":"Jl. Melati 1/2 – kamar <A>"}`,
			wantKeys: []string{"name"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, keys, fired, changed := m.maskJSON(tt.in)
			want := tt.want
			if want == "" {
				want = tt.in
			}
			if out != want {
				t.Errorf("out = %s\nwant  %s", out, want)
			}
			if changed != (tt.want != "") {
				t.Errorf("changed = %v, want %v", changed, tt.want != "")
			}
			if !reflect.DeepEqual(keys, tt.wantKeys) {
				t.Errorf("keys = %v, want %v", keys, tt.wantKeys)
			}
			if !reflect.DeepEqual(fired, tt.wantFired) {
				t.Errorf("fired = %v, want %v", fired, tt.wantFired)
			}
			if strings.Contains(out, `\u003c`) {
				t.Errorf("out escaped the placeholder: %s", out)
			}
			if tt.wantKeys != nil && !strings.Contains(out, maskedValue) {
				t.Errorf("out has no literal %s: %s", maskedValue, out)
			}
		})
	}
}

func TestScansJSON(t *testing.T) {
	var none *Masker
	if none.scansJSON() {
		t.Error("a nil masker scans JSON")
	}
	cases := []struct {
		name string
		mask []string
		want bool
	}{
		{"bare rule", []string{"phone"}, true},
		{"glob rule", []string{"*_phone"}, true},
		{"qualified only", []string{"users.phone", "orders.*"}, false},
		// A table glob that matches the empty string reaches keys through
		// matchAny, so the walk must run for it.
		{"star table", []string{"*.phone"}, true},
		{"mixed", []string{"users.phone", "email"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := jsonMasker(t, &MaskingConfig{Mask: tc.mask})
			if got := m.scansJSON(); got != tc.want {
				t.Errorf("scansJSON = %v, want %v", got, tc.want)
			}
		})
	}
}

// With only qualified rules the cell is not parsed at all: the raw detectors
// are what see it, and they do so from the row loop, not from here.
func TestMaskJSONSkipsWithoutKeyRules(t *testing.T) {
	m := jsonMasker(t, &MaskingConfig{Mask: []string{"users.phone"}, Values: []string{"email"}})
	in := `{"phone":"0812","note":"a@b.com"}`
	if out, _, _, changed := m.maskJSON(in); changed || out != in {
		t.Errorf("maskJSON parsed a cell no key rule can reach: %q", out)
	}
	if out, fired := m.scanValue(in); fired == nil || !strings.Contains(out, maskedValue) {
		t.Errorf("the raw scan missed the email: %q", out)
	}
}

// The star-table rule is the one case where a qualified spelling reaches a
// key, because * matches the empty table a key has.
func TestMaskJSONStarTableRule(t *testing.T) {
	m := jsonMasker(t, &MaskingConfig{Mask: []string{"*.phone"}})
	out, keys, _, changed := m.maskJSON(`{"phone":"0812"}`)
	if !changed || out != `{"phone":"`+maskedValue+`"}` || len(keys) != 1 {
		t.Errorf("out = %q keys = %v changed = %v", out, keys, changed)
	}
}

func TestJSONKeyNote(t *testing.T) {
	got := jsonKeyNote(map[string][]string{
		"props": {"phone", "name"},
		"extra": {"name"},
	})
	want := `JSON keys name, phone inside extra, props are "<masked>" by server PII policy`
	if got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}
