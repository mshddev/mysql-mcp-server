package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Every HTTP caller presents a bearer token, and every token belongs to a
// name: server.auth_tokens maps one to the other. The name is what the query
// log records as "caller", which is the whole reason tokens are per person
// rather than one for the team. A team that wants a single shared token
// defines one name and hands its token around; the log then names the team.

// bearerAuth guards next with the token set. A request whose token is not in
// the set gets 401 and never reaches next; one that matches carries its
// caller's name into the MCP handler, which the go-sdk hands to the tool as
// req.Extra.TokenInfo.UserID.
func bearerAuth(tokens map[string]string, next http.Handler) http.Handler {
	verifier := func(_ context.Context, presented string, r *http.Request) (*auth.TokenInfo, error) {
		name, ok := lookupToken(tokens, presented)
		if !ok {
			// A stale token from a real client is worth a line: the
			// address says who to talk to. Nothing from the token itself
			// is logged, a near-miss is still a secret.
			slog.Warn("auth", "error", "unknown token", "remote", r.RemoteAddr)
			return nil, fmt.Errorf("%w: unauthorized", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{UserID: name}, nil
	}
	// The tokens are static secrets with no expiry to check.
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
}

// lookupToken finds the name whose token is presented. Every entry is
// compared, in constant time, whether or not an earlier one matched, so the
// time taken says nothing about which entry, if any, was the right one.
// Tokens are unique (LoadConfig checks), so at most one entry matches.
func lookupToken(tokens map[string]string, presented string) (string, bool) {
	var name string
	found := 0
	for n, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(presented)) == 1 {
			name = n
			found = 1
		}
	}
	return name, found == 1
}

// callerName is the caller the request authenticated as, or "" under stdio,
// where no token was checked: the caller is whoever launched the process.
func callerName(info *auth.TokenInfo) string {
	if info == nil {
		return ""
	}
	return info.UserID
}
