package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTunnelRewriteScriptNoCookieSideEffects(t *testing.T) {
	if strings.Contains(tunnelRewriteScriptTemplate, `document.cookie`) {
		t.Error("rewrite script must not set document.cookie — root-path cookie overwrites across tunnels")
	}
	if strings.Contains(tunnelRewriteScriptTemplate, "visibilitychange") {
		t.Error("rewrite script must not register a visibilitychange listener")
	}
}

func TestRootHandlerSingleModeNoTunnel(t *testing.T) {
	hub := NewHub()
	metrics := NewMetrics()
	cfg := Config{SingleTunnelMode: true}
	handler := rootHandler(hub, metrics, cfg)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No tunnel connected") {
		t.Errorf("expected 'No tunnel connected' in body, got: %s", w.Body.String())
	}
}

func TestRootHandlerSingleModeWithTunnel(t *testing.T) {
	hub := NewHub()
	hub.Register(closedTunnel("abc123"))
	metrics := NewMetrics()
	cfg := Config{SingleTunnelMode: true}
	handler := rootHandler(hub, metrics, cfg)

	req := httptest.NewRequest("GET", "/some/path", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Closed tunnel returns 502 (tunnel disconnected), not a redirect or 503.
	if w.Code == http.StatusServiceUnavailable {
		t.Error("should not return 503 when tunnel is active")
	}
	if w.Code == http.StatusTemporaryRedirect || w.Code == http.StatusMovedPermanently {
		t.Errorf("single mode must proxy directly, not redirect; got %d", w.Code)
	}
}

func TestRootHandlerMultiModeNoContext(t *testing.T) {
	hub := NewHub()
	hub.Register(closedTunnel("abc123"))
	metrics := NewMetrics()
	cfg := Config{SingleTunnelMode: false}
	handler := rootHandler(hub, metrics, cfg)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 tunnel list, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "abc123") {
		t.Errorf("expected tunnel ID 'abc123' in list body, got: %s", w.Body.String())
	}
}

func TestRootHandlerMultiModeNoTunnels(t *testing.T) {
	hub := NewHub()
	metrics := NewMetrics()
	cfg := Config{SingleTunnelMode: false}
	handler := rootHandler(hub, metrics, cfg)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 empty state, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No tunnel connected") {
		t.Errorf("expected empty state message in body, got: %s", w.Body.String())
	}
}

func TestRootHandlerMultiModeWithReferer(t *testing.T) {
	hub := NewHub()
	hub.Register(closedTunnel("abc123"))
	metrics := NewMetrics()
	cfg := Config{SingleTunnelMode: false}
	handler := rootHandler(hub, metrics, cfg)

	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Referer", "https://example.com/t/abc123/dashboard")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307 redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/t/abc123/") {
		t.Errorf("expected redirect to /t/abc123/..., got %q", loc)
	}
}
