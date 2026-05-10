package server

import (
	"crypto/subtle"
	"os"
	"strings"
)

// Auth validates tokens against configured auth tokens.
type Auth struct {
	tokens [][]byte
}

// NewAuth creates an Auth from the TUNNEL_AUTH_TOKENS environment variable.
// If no tokens are configured, all requests are allowed.
func NewAuth() *Auth {
	a := &Auth{}
	raw := os.Getenv("TUNNEL_AUTH_TOKENS")
	if raw == "" {
		return a
	}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			a.tokens = append(a.tokens, []byte(tok))
		}
	}
	return a
}

// Validate checks if a token is valid. Returns true if no tokens are configured
// (open mode) or if the token matches one of the configured tokens. Comparison
// is constant-time per token to avoid timing side channels.
func (a *Auth) Validate(token string) bool {
	if len(a.tokens) == 0 {
		return true
	}
	tb := []byte(token)
	var ok byte
	for _, t := range a.tokens {
		ok |= byte(subtle.ConstantTimeCompare(tb, t))
	}
	return ok == 1
}
