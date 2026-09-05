package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Value masking is the second layer under the column rules: it looks at the
// shape of a string cell rather than where the column came from, so personal
// data that lives in columns no rule can name (free text, JSON blobs, a view's
// renamed column, a table copied under full_access) still gets hidden. It is
// strictly additive — it never lowers a column decision, never refuses a
// query, and only masks more — and it is best-effort by nature: only values
// with a recognisable shape (an email address, an Indonesian phone number) can
// be caught. A name or an address has no shape, so the column rules stay the
// primary mechanism.

// valueDetector finds one kind of personal data inside a string. Detectors are
// picked by name in masking.values; there is deliberately no operator-supplied
// regex, since a wrong one either leaks or masks the whole result.
type valueDetector struct {
	name string
	find func(s string) [][2]int // byte spans to mask, ascending, non-overlapping
}

// detectors is the whole catalogue, in the order they run. Email goes first so
// a digit-heavy local part is masked as an email rather than sliced by the
// phone detector.
var detectors = []valueDetector{
	{name: "email", find: findEmails},
	{name: "phone_id", find: findIndonesianPhones},
}

func detectorNames() []string {
	names := make([]string, len(detectors))
	for i, d := range detectors {
		names[i] = d.name
	}
	return names
}

// parseDetectors resolves masking.values names, keeping the catalogue order so
// the run order does not depend on how the config was written.
func parseDetectors(entries []string) ([]valueDetector, error) {
	wanted := map[string]bool{}
	for _, e := range entries {
		name := strings.ToLower(strings.TrimSpace(e))
		known := false
		for _, d := range detectors {
			if d.name == name {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("masking.values entry %q: unknown detector (known: %s)",
				e, strings.Join(detectorNames(), ", "))
		}
		wanted[name] = true
	}
	var out []valueDetector
	for _, d := range detectors {
		if wanted[d.name] {
			out = append(out, d)
		}
	}
	return out, nil
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

func findEmails(s string) [][2]int {
	var spans [][2]int
	for _, m := range emailRe.FindAllStringIndex(s, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	return spans
}

// Indonesian mobile numbers: 08 or the country form 62 / +62, then 8, then
// 8–11 more digits, optionally separated by single spaces, dots, or dashes
// (0812-3456-7890, +62 812-3456-7890). A separator is allowed after the
// country code only: "0.812345678" is a DECIMAL, not a phone.
//
// Go's RE2 has no look-around, so the trailing "not glued to more digits"
// boundary is part of the match ([^0-9]|$) and the number itself is group 1.
// Consuming the boundary matters: a greedy match that was merely rejected
// afterwards would skip the phone in "081234567890 2026" entirely, because
// the separator-digit repetition swallows " 2" and then sees a digit. The
// leading boundary is checked in code against the original string.
var phoneIDRe = regexp.MustCompile(`((?:\+?62[ .-]?|0)8(?:[ .-]?[0-9]){8,11})(?:[^0-9]|$)`)

func findIndonesianPhones(s string) [][2]int {
	var spans [][2]int
	for _, m := range phoneIDRe.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[2], m[3]
		if start > 0 && isDigit(s[start-1]) {
			continue // the tail of a longer number, not a phone
		}
		spans = append(spans, [2]int{start, end})
	}
	return spans
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// scanValue runs every enabled detector over one string cell and returns the
// masked text plus the names of the detectors that fired, or the input and nil
// when nothing matched. Each detector sees the output of the previous one, so
// an already-masked span is never re-matched.
func (m *Masker) scanValue(s string) (string, []string) {
	if m == nil || len(m.values) == 0 || s == "" {
		return s, nil
	}
	var fired []string
	for _, d := range m.values {
		spans := d.find(s)
		if len(spans) == 0 {
			continue
		}
		fired = append(fired, d.name)
		var b strings.Builder
		b.Grow(len(s))
		prev := 0
		for _, sp := range spans {
			b.WriteString(s[prev:sp[0]])
			b.WriteString(maskedValue)
			prev = sp[1]
		}
		b.WriteString(s[prev:])
		s = b.String()
	}
	return s, fired
}

// scansValues reports whether any value detector is enabled, so the row loop
// can skip the per-cell work entirely when the feature is off.
func (m *Masker) scansValues() bool { return m != nil && len(m.values) > 0 }

// valueHits accumulates, per result column, which detectors fired on at least
// one of its cells.
type valueHits map[int]map[string]bool

func (h valueHits) add(col int, names []string) {
	// Nothing fired means no entry: a column map created for an empty list
	// would be reported as a column with no detectors, and grow a note
	// sentence to match. The JSON walker feeds this with nil whenever only
	// keys matched.
	if len(names) == 0 {
		return
	}
	if h[col] == nil {
		h[col] = map[string]bool{}
	}
	for _, n := range names {
		h[col][n] = true
	}
}

// report turns the hits into the response shape: result label → detector
// names, sorted so the output is stable.
func (h valueHits) report(labels []string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for col, names := range h {
		list := make([]string, 0, len(names))
		for n := range names {
			list = append(list, n)
		}
		sort.Strings(list)
		out[labelAt(labels, col)] = list
	}
	return out
}
