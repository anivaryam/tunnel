package main

import (
	"bytes"
	"os"
	"testing"
)

func TestServerDoesNotSetWriteTimeout(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(src, []byte("WriteTimeout:")) {
		t.Fatal("server must not set WriteTimeout; long-lived streams need open-ended writes")
	}
}

func TestRequireAuthConfigRejectsEmptyTokens(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "")
	t.Setenv("TUNNEL_ALLOW_OPEN", "")

	if err := requireAuthConfig(); err == nil {
		t.Fatal("expected missing auth tokens to be rejected")
	}
}

func TestRequireAuthConfigAllowsExplicitOpenMode(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "")
	t.Setenv("TUNNEL_ALLOW_OPEN", "true")

	if err := requireAuthConfig(); err != nil {
		t.Fatalf("expected explicit open mode to pass: %v", err)
	}
}

func TestRequireAuthConfigAllowsTokens(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "secret")
	t.Setenv("TUNNEL_ALLOW_OPEN", "")

	if err := requireAuthConfig(); err != nil {
		t.Fatalf("expected configured tokens to pass: %v", err)
	}
}
