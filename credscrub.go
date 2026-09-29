package main

import (
	"regexp"
	"strings"
)

// Credential scrubbing keeps the database credential out of what the server
// hands back and writes down. MariaDB's SHOW GRANTS and SHOW CREATE USER
// print the connected user's password hash inside an auth clause, and
// mysql_native_password hashes are unsalted double SHA1, so a weak password
// falls to an offline crack; MySQL 8's SHOW CREATE USER prints its own
// hashes the same way, and a CREATE USER or ALTER USER statement carries a
// plain password into the query log. The scrub replaces the secret in each
// such clause with <masked> and leaves the rest of the text alone, so SHOW
// GRANTS still answers what the user may do.
//
// It is always on and has nothing to do with the masking section: no config
// turns it off, masking.except does not shield a cell from it, and it runs in
// both modes and over both transports. It works on text, so a hash read
// straight out of mysql.user is a plain column value it cannot recognise;
// that is a column rule's job (authentication_string in the starter list).

// credentialDetector is the name a scrubbed cell is reported under in
// masked_values, beside the masking.values detectors. It is not one of them:
// masking.values does not accept it, since there is nothing to switch on.
const credentialDetector = "credential"

const (
	// sqlString is a quoted SQL string in either quote style, with backslash
	// and doubled-quote escapes. One that never closes runs to the end of the
	// text instead, because SHOW PROCESSLIST cuts a statement at 100
	// characters and a secret cut short is still a secret. The closed forms
	// come first so they win whenever the text has one: a database error
	// quoting `IDENTIFIED BY 'pw'` ends in `'pw'' at line 1`, and only the
	// first two quotes there belong to the value.
	sqlString = `(?:'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.|"")*"|'(?:[^'\\]|\\.|'')*$|"(?:[^"\\]|\\.|"")*$)`

	// sqlHex is a hex literal, the form MySQL 8 prints a hash in under
	// print_identified_with_as_hex.
	sqlHex = `0x[0-9a-f]+|x'[0-9a-f]*'`

	// authSecret is what an auth clause carries after BY, AS or USING: a
	// string, optionally in MariaDB's PASSWORD('…') or the PASSWORD '…' of a
	// hash, or a hex literal. MySQL 8's RANDOM PASSWORD has no secret in it,
	// but a REPLACE '<current password>' can follow it.
	authSecret = `(?:(?:password\b\s*\(?\s*)?` + sqlString + `\s*\)?|` + sqlHex + `|random\s+password\b)`

	// replaceTail is MySQL 8's REPLACE '<current password>', which follows
	// the new one when the account must prove it knows the old.
	replaceTail = `(?:\s*\breplace\b\s*` + sqlString + `)?`

	// authMethod is one plugin with its optional secret. MariaDB chains
	// several with OR (IDENTIFIED VIA ed25519 USING '…' OR unix_socket), and
	// either end of a chain may be the one without a secret.
	authMethod = "(?:[a-z0-9_$]+|" + sqlString + "|`[^`]*`)" +
		`(?:\s*\b(?:as|using|by)\b\s*` + authSecret + replaceTail + `)?`
)

// authClauseRe finds a whole auth clause, OR chain included, so the secrets
// inside it can be told apart from the plugin names around them:
//
//	IDENTIFIED BY [PASSWORD] '…' [REPLACE '…']       MariaDB hash; plain password
//	IDENTIFIED BY RANDOM PASSWORD [REPLACE '…']      MySQL 8
//	IDENTIFIED {WITH|VIA} plugin [{AS|USING|BY} secret] [OR plugin …]
var authClauseRe = regexp.MustCompile(`(?is)\bidentified\s+(?:by\b\s*` + authSecret + replaceTail +
	`|(?:with|via)\b\s*` + authMethod + `(?:\s*\bor\b\s*` + authMethod + `)*)`)

// setPasswordRe finds SET PASSWORD [FOR user] = … or TO RANDOM, with the
// REPLACE that may follow. Only a statement carries one, so only the log
// path looks for it.
var setPasswordRe = regexp.MustCompile(`(?is)\bset\s+password\b[^=]*?(?:=\s*(?:(?:old_)?password\b\s*\(?\s*)?` +
	sqlString + `|\bto\s+random\b)` + replaceTail)

// secretRe finds each secret inside a clause the two regexps above matched:
// the value after a keyword that introduces one, or after SET PASSWORD's
// "=". Group 1 is the secret; the keyword and any PASSWORD( before it stay.
var secretRe = regexp.MustCompile(`(?is)(?:=|\b(?:by|as|using|replace)\b)\s*(?:(?:old_)?password\b\s*\(?\s*)?(` +
	sqlString + `|` + sqlHex + `)`)

// scrubCredentials replaces the secret in every auth clause in s with
// <masked>, and returns the result with the secrets it replaced, or s itself
// and nil when there were none. Text without the word "identified" is
// passed over before any regexp runs, which is nearly every cell.
func scrubCredentials(s string) (string, []string) {
	if !containsFold(s, "identified") {
		return s, nil
	}
	return maskSecrets(s, authClauseRe)
}

// scrubStatement is scrubCredentials for the logged copy of a statement,
// which also covers SET PASSWORD.
func scrubStatement(sql string) (string, []string) {
	out, secrets := scrubCredentials(sql)
	if containsFold(out, "password") {
		var more []string
		out, more = maskSecrets(out, setPasswordRe)
		secrets = append(secrets, more...)
	}
	return out, secrets
}

// scrubEcho scrubs the logged copy of text that may quote the statement back
// at the caller, a database error above all. MySQL and the masking parser
// both quote from the fault onward, which can start after IDENTIFIED and
// leave the secret with nothing to recognise it by, so the secrets scrubbed
// from the statement itself are removed from s verbatim before the patterns
// run. An empty password hides nothing and is left alone, since replacing
// its pair of quotes would only mangle the rest of the message.
func scrubEcho(s string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) > 2 {
			s = strings.ReplaceAll(s, secret, maskedValue)
		}
	}
	s, _ = scrubStatement(s)
	return s
}

// maskSecrets masks every secret inside every match of clause in s.
func maskSecrets(s string, clause *regexp.Regexp) (string, []string) {
	var b strings.Builder
	var secrets []string
	prev := 0
	for _, c := range clause.FindAllStringIndex(s, -1) {
		for _, m := range secretRe.FindAllStringSubmatchIndex(s[c[0]:c[1]], -1) {
			start, end := c[0]+m[2], c[0]+m[3]
			b.WriteString(s[prev:start])
			b.WriteString(maskedValue)
			secrets = append(secrets, s[start:end])
			prev = end
		}
	}
	if secrets == nil {
		return s, nil
	}
	b.WriteString(s[prev:])
	return b.String(), secrets
}

// containsFold reports whether s contains word, a lowercase ASCII word, in
// any letter case. It allocates nothing, unlike lowercasing s first, which
// matters because every text cell of every result goes through it. OR-ing in
// 0x20 lowercases an ASCII letter and turns no other byte into one, so the
// comparison is exact for a word made of letters.
func containsFold(s, word string) bool {
next:
	for i := 0; i+len(word) <= len(s); i++ {
		for j := 0; j < len(word); j++ {
			if s[i+j]|0x20 != word[j] {
				continue next
			}
		}
		return true
	}
	return false
}
