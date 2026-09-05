package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path"
	"sort"
	"strings"
)

// JSON masking applies the column rules one level in. An activity log, an
// audit trail, a request dump — the usual JSON-valued column — carries the
// row it describes as keys ({"attributes":{"name":…,"phone":…}}), and the
// column's own name matches no rule while a name or an address has no shape
// for the value detectors. So a bare mask rule ("phone", "*_email") is taken
// to name a key as well as a column: inside a JSON cell, a matching key's
// value comes back as "<masked>" whole, nested object or array included. It
// is strictly additive like the value layer, and it is not a separate switch
// (forgetting a flag is the failure mode): whoever masks a column named
// "name" gets "name" keys masked too. The price is over-masking — a "name"
// key in a room's log entry is hidden as readily as a tenant's — and the
// carve-out is masking.except on the column itself, which shields the cell
// from every layer.

// scansJSON reports whether any mask rule can apply to a JSON key. A key has
// no table, so the qualified rules never reach it ("users.phone" is a column
// rule only) and the walk is skipped when there are none but those; the raw
// value detectors still see the cell's text. A table glob that matches the
// empty string ("*.phone") does reach keys, because matchAny is what decides.
func (m *Masker) scansJSON() bool { return m != nil && m.jsonKeys }

func rulesReachKeys(rules []maskRule) bool {
	for _, r := range rules {
		if r.table == "" {
			return true
		}
		if ok, _ := path.Match(r.table, ""); ok {
			return true
		}
	}
	return false
}

// keyMasked applies the rules to one JSON key, case-insensitively like a
// column name. except beats mask, as everywhere.
func (m *Masker) keyMasked(key string) bool {
	key = strings.ToLower(key)
	return !matchAny(m.except, "", key) && matchAny(m.mask, "", key)
}

// keyExcepted is the JSON counterpart of Excepted: an excepted key's subtree
// is trusted outright, and the value detectors stay out of it too.
func (m *Masker) keyExcepted(key string) bool {
	return matchAny(m.except, "", strings.ToLower(key))
}

// maskJSON reads one string cell as JSON and returns it with matching keys
// masked and detectors run over the remaining string leaves, plus the key
// names and detector names that fired. changed is false — and out is the
// input, byte for byte — when the cell is not JSON, when no rule can reach a
// key, or when nothing fired: re-serialising reorders object keys, so a cell
// nothing touched must not be rewritten. The caller then treats the cell as
// the plain text it always was.
func (m *Masker) maskJSON(s string) (out string, keys, fired []string, changed bool) {
	if !m.scansJSON() {
		return s, nil, nil, false
	}
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return s, nil, nil, false
	}
	// UseNumber keeps an id past 2^53 as its digits; a float64 round-trip
	// would rewrite it. Trailing content after the value is checked because
	// Decode stops at the value's end and would otherwise drop the rest.
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s, nil, nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return s, nil, nil, false
	}

	w := &jsonWalker{m: m, keys: map[string]bool{}, fired: map[string]bool{}}
	v = w.walk(v)
	if len(w.keys) == 0 && len(w.fired) == 0 {
		return s, nil, nil, false
	}
	// The default encoder writes "<" as <, which would turn the
	// placeholder into something no reader recognises.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return s, nil, nil, false
	}
	return strings.TrimSuffix(buf.String(), "\n"), sortedKeys(w.keys), sortedKeys(w.fired), true
}

type jsonWalker struct {
	m     *Masker
	keys  map[string]bool
	fired map[string]bool
}

func (w *jsonWalker) walk(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			switch {
			case w.m.keyExcepted(k):
				// Trusted subtree, left exactly as it came.
			case w.m.keyMasked(k):
				// NULL stays null for the same reason a masked column's
				// does: that a value is absent is not personal data.
				if child != nil {
					x[k] = maskedValue
					w.keys[k] = true
				}
			default:
				x[k] = w.walk(child)
			}
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = w.walk(child)
		}
		return x
	case string:
		out, fired := w.m.scanValue(x)
		for _, n := range fired {
			w.fired[n] = true
		}
		return out
	default:
		// Numbers (json.Number), booleans, null.
		return v
	}
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonKeyNote words the masked_json_keys map for the note, stable like
// valueMaskNote.
func jsonKeyNote(mk map[string][]string) string {
	cols := make([]string, 0, len(mk))
	keys := map[string]bool{}
	for col, names := range mk {
		cols = append(cols, col)
		for _, n := range names {
			keys[n] = true
		}
	}
	sort.Strings(cols)
	return "JSON keys " + strings.Join(sortedKeys(keys), ", ") + " inside " +
		strings.Join(cols, ", ") + " are " + `"` + maskedValue + `"` + " by server PII policy"
}
