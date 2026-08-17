package main

import (
	"strings"
	"testing"
)


func TestNewMasker(t *testing.T) {
	tests := []struct {
		name    string
		mc      *MaskingConfig
		wantNil bool
		wantErr string
	}{
		{name: "absent section", mc: nil, wantNil: true},
		{
			name:    "rules with enabled omitted are active",
			mc:      &MaskingConfig{Mask: []string{"phone"}},
			wantNil: false,
		},
		{
			name:    "explicitly disabled",
			mc:      &MaskingConfig{Enabled: new(false), Mask: []string{"phone"}},
			wantNil: true,
		},
		{
			name:    "enabled without rules",
			mc:      &MaskingConfig{Enabled: new(true)},
			wantErr: "no rules",
		},
		{
			name:    "empty section counts as enabled without rules",
			mc:      &MaskingConfig{},
			wantErr: "no rules",
		},
		{
			// Broken rules must fail the load even while disabled, or flipping
			// enabled on later would surprise with a config error.
			name:    "disabled still validates rules",
			mc:      &MaskingConfig{Enabled: new(false), Mask: []string{"[bad"}},
			wantErr: "[bad",
		},
		{
			name:    "bad glob",
			mc:      &MaskingConfig{Mask: []string{"phone", "[bad"}},
			wantErr: "masking.mask entry \"[bad\"",
		},
		{
			name:    "bad glob in except",
			mc:      &MaskingConfig{Mask: []string{"phone"}, Except: []string{"[bad"}},
			wantErr: "masking.except entry",
		},
		{
			name:    "empty entry",
			mc:      &MaskingConfig{Mask: []string{""}},
			wantErr: "empty column pattern",
		},
		{
			name:    "empty column part",
			mc:      &MaskingConfig{Mask: []string{"users."}},
			wantErr: "empty column pattern",
		},
		{
			name:    "empty table part",
			mc:      &MaskingConfig{Mask: []string{".phone"}},
			wantErr: "empty table part",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewMasker(tt.mc)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("NewMasker succeeded, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMasker: %v", err)
			}
			if (m == nil) != tt.wantNil {
				t.Errorf("masker = %v, want nil: %v", m, tt.wantNil)
			}
		})
	}
}

func TestMaskerMasked(t *testing.T) {
	m, err := NewMasker(&MaskingConfig{
		Mask:   []string{"phone", "*_phone", "users.address", "display_name"},
		Except: []string{"room_types.display_name"},
	})
	if err != nil {
		t.Fatalf("NewMasker: %v", err)
	}

	tests := []struct {
		name     string
		orgTable string
		orgName  string
		want     bool
	}{
		{name: "bare rule matches in any table", orgTable: "users", orgName: "phone", want: true},
		{name: "bare rule matches elsewhere too", orgTable: "owners", orgName: "phone", want: true},
		{name: "glob rule", orgTable: "bookings", orgName: "owner_phone", want: true},
		{name: "glob needs the suffix", orgTable: "bookings", orgName: "phones", want: false},
		{name: "qualified rule in its table", orgTable: "users", orgName: "address", want: true},
		{name: "qualified rule elsewhere", orgTable: "bookings", orgName: "address", want: false},
		{name: "case-insensitive name", orgTable: "users", orgName: "PHONE", want: true},
		{name: "case-insensitive table", orgTable: "USERS", orgName: "Address", want: true},
		{name: "unlisted column", orgTable: "users", orgName: "id", want: false},
		{name: "except beats mask", orgTable: "room_types", orgName: "display_name", want: false},
		{name: "except only in its table", orgTable: "users", orgName: "display_name", want: true},
		{
			// A column may carry an origin name without a table; bare rules
			// must still catch it.
			name: "empty table with a bare rule", orgTable: "", orgName: "phone", want: true,
		},
		{name: "empty table never matches a qualified rule", orgTable: "", orgName: "address", want: false},
		{
			// No origin name means Masked can't vouch either way and passes it
			// through — planQuery only routes queries here whose wire origins
			// are trustworthy, and hides untraceable columns itself.
			name: "no origin passes through", orgTable: "users", orgName: "", want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.Masked([]byte(tt.orgTable), []byte(tt.orgName)); got != tt.want {
				t.Errorf("Masked(%q, %q) = %v, want %v", tt.orgTable, tt.orgName, got, tt.want)
			}
		})
	}
}

func TestMaskerNilMasksNothing(t *testing.T) {
	var m *Masker
	if m.Masked([]byte("users"), []byte("phone")) {
		t.Error("nil masker masked a column, want everything passed through")
	}
}
