package main

import (
	"fmt"
	"path"
	"strings"
)

// maskedValue replaces every non-NULL cell of a masked column. NULL stays
// null: whether a value exists is not PII, the value is.
const maskedValue = "<masked>"

// maskRule is one parsed config entry. Both parts are lowercase globs; an
// empty table means "any table".
type maskRule struct {
	table  string
	column string
}

// Masker holds the parsed masking rules. A nil Masker masks nothing. When a
// Masker exists, every query is verified by reading it (planQuery in
// strictmask.go); rule matching against a wire-protocol origin (Masked) is the
// fast path for queries simple enough that the wire tag is trustworthy.
type Masker struct {
	mask   []maskRule
	except []maskRule
}

// NewMasker parses the masking config into a Masker, or nil when the section
// is absent or explicitly disabled. Rules are validated even when disabled,
// so flipping enabled on later cannot surprise with a config error.
func NewMasker(mc *MaskingConfig) (*Masker, error) {
	if mc == nil {
		return nil, nil
	}
	mask, err := parseRules("masking.mask", mc.Mask)
	if err != nil {
		return nil, err
	}
	except, err := parseRules("masking.except", mc.Except)
	if err != nil {
		return nil, err
	}
	// An omitted enabled flag means true: whoever wrote rules wants them
	// active, and defaulting the other way would disable masking silently.
	if mc.Enabled != nil && !*mc.Enabled {
		return nil, nil
	}
	if len(mask) == 0 {
		return nil, fmt.Errorf("masking is enabled but masking.mask has no rules; set masking.enabled: false to opt out")
	}
	return &Masker{mask: mask, except: except}, nil
}

func parseRules(list string, entries []string) ([]maskRule, error) {
	rules := make([]maskRule, 0, len(entries))
	for _, e := range entries {
		r, err := parseRule(e)
		if err != nil {
			return nil, fmt.Errorf("%s entry %q: %w", list, e, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// parseRule accepts "pattern" (any table) or "table.pattern". Both sides are
// case-insensitive globs (*, ?, [...]). A rule that could never match
// anything is a config error, not a silent no-op.
func parseRule(s string) (maskRule, error) {
	r := maskRule{column: strings.ToLower(s)}
	if table, column, qualified := strings.Cut(s, "."); qualified {
		r.table = strings.ToLower(table)
		r.column = strings.ToLower(column)
		if r.table == "" {
			return maskRule{}, fmt.Errorf("empty table part")
		}
	}
	if r.column == "" {
		return maskRule{}, fmt.Errorf("empty column pattern")
	}
	for _, pat := range []string{r.table, r.column} {
		if _, err := path.Match(pat, ""); err != nil {
			return maskRule{}, fmt.Errorf("bad glob %q: %w", pat, err)
		}
	}
	return r, nil
}

// Masked reports whether a column's values must be masked, given its
// wire-protocol origin. Columns with no origin (expressions, aggregates) pass
// through: the wire cannot say what fed them, and failing closed there would
// mask COUNT(*).
func (m *Masker) Masked(orgTable, orgName []byte) bool {
	if m == nil || len(orgName) == 0 {
		return false
	}
	table := strings.ToLower(string(orgTable))
	column := strings.ToLower(string(orgName))
	return !matchAny(m.except, table, column) && matchAny(m.mask, table, column)
}

func matchAny(rules []maskRule, table, column string) bool {
	for _, r := range rules {
		// Patterns were validated at config load, so Match cannot error here.
		if ok, _ := path.Match(r.column, column); !ok {
			continue
		}
		if r.table == "" {
			return true
		}
		if ok, _ := path.Match(r.table, table); ok {
			return true
		}
	}
	return false
}
