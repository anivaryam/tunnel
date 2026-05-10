package server

import (
	"testing"
)

func TestNewAuthEmpty(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "")
	a := NewAuth()

	if !a.Validate("any-token") {
		t.Error("Validate() should return true for any token when no tokens configured")
	}
	if !a.Validate("") {
		t.Error("Validate() should return true for empty token when no tokens configured")
	}
}

func TestNewAuthSingleToken(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "secret-token")
	a := NewAuth()

	if !a.Validate("secret-token") {
		t.Error("Validate() should return true for configured token")
	}
	if a.Validate("wrong-token") {
		t.Error("Validate() should return false for non-configured token")
	}
}

func TestNewAuthMultipleTokens(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "token1,token2,token3")
	a := NewAuth()

	if !a.Validate("token1") {
		t.Error("Validate() should return true for token1")
	}
	if !a.Validate("token2") {
		t.Error("Validate() should return true for token2")
	}
	if !a.Validate("token3") {
		t.Error("Validate() should return true for token3")
	}
	if a.Validate("invalid") {
		t.Error("Validate() should return false for invalid token")
	}
}

func TestNewAuthTrimSpace(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", " token1 , token2 , token3 ")
	a := NewAuth()

	if !a.Validate("token1") {
		t.Error("Validate() should return true for trimmed token1")
	}
	if !a.Validate("token2") {
		t.Error("Validate() should return true for trimmed token2")
	}
	if !a.Validate("token3") {
		t.Error("Validate() should return true for trimmed token3")
	}
	if a.Validate(" token1") {
		t.Error("Validate() should return false for token with leading space")
	}
}

func TestValidateEmptyToken(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "token1,token2")
	a := NewAuth()

	if a.Validate("") {
		t.Error("Validate() should return false for empty token when tokens are configured")
	}
}

func TestValidateInvalidToken(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "valid-token")
	a := NewAuth()

	if a.Validate("invalid-token") {
		t.Error("Validate() should return false for invalid token")
	}
	if a.Validate("Valid-Token") {
		t.Error("Validate() should return false for case-mismatched token")
	}
	if a.Validate("valid-token-extra") {
		t.Error("Validate() should return false for partial token")
	}
}
