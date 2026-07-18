package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/coder/websocket"
)

func testSubdomainHandler(cfg Config) (http.Handler, *Hub) {
	hub := NewHub()
	metrics := NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fallback", http.StatusNotFound)
	})
	return subdomainMiddleware(mux, hub, metrics, cfg), hub
}

// closedTunnel creates a tunnel that immediately returns "tunnel closed" on any
// sendRequest call, so tests can verify routing without a real WebSocket connection.
func closedTunnel(id string) *Tunnel {
	t := NewTunnel(id, "http", nil)
	t.Close()
	return t
}

func TestSubdomainMiddlewareWorkerSecretRoutes(t *testing.T) {
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret", CFWorkerEnabled: true}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "myapp.tunnel.example.com"
	req.Header.Set("X-Forwarded-Tunnel", "myapp")
	req.Header.Set("X-Worker-Secret", "testsecret")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Closed tunnel returns "tunnel disconnected" (502), not the fallback "fallback" (404).
	if strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("expected tunnel routing, got fallback; status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestSubdomainMiddlewareCFWorkerWrongHostRejected(t *testing.T) {
	// Even with the correct WORKER_SECRET, a request whose Host does not end
	// in BASE_DOMAIN must NOT be routed via the CF path (closes the lateral
	// movement window where a leaked secret could route arbitrary subdomains).
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret", CFWorkerEnabled: true}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.attacker.com"
	req.Header.Set("X-Forwarded-Tunnel", "myapp")
	req.Header.Set("X-Worker-Secret", "testsecret")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("expected fallback (Host suffix mismatch should reject CF path); status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestSubdomainMiddlewareWrongSecretFallsThrough(t *testing.T) {
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret", CFWorkerEnabled: true}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "tunnel.example.com"
	req.Header.Set("X-Forwarded-Tunnel", "myapp")
	req.Header.Set("X-Worker-Secret", "wrongsecret")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 fallback with wrong secret, got %d: %q", w.Code, w.Body.String())
	}
}

func TestSubdomainMiddlewareEmptySecretIgnoresHeader(t *testing.T) {
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: ""}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	// X-Forwarded-Tunnel present but WorkerSecret is empty — header must be ignored.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "tunnel.example.com"
	req.Header.Set("X-Forwarded-Tunnel", "myapp")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 fallback when WorkerSecret is empty, got %d", w.Code)
	}
}

func TestSubdomainMiddlewareHostRoutingUnchanged(t *testing.T) {
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret"}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	// Direct subdomain access without worker headers — must still route via Host.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "myapp.tunnel.example.com"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("expected host-based routing to reach the tunnel, got fallback; body=%q", w.Body.String())
	}
}

func TestSubdomainMiddlewareUnknownTunnel502(t *testing.T) {
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret", CFWorkerEnabled: true}
	handler, _ := testSubdomainHandler(cfg)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "nonexistent.tunnel.example.com"
	req.Header.Set("X-Forwarded-Tunnel", "nonexistent")
	req.Header.Set("X-Worker-Secret", "testsecret")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for unknown tunnel, got %d", w.Code)
	}
}

func TestSubdomainMiddlewareDirectDNSWithCFWorkerDisabled(t *testing.T) {
	// CFWorkerEnabled=false means Cloudflare Worker routing is disabled.
	// Direct DNS subdomain routing should still work via Host header.
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "", CFWorkerEnabled: false}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "myapp.tunnel.example.com"
	// No X-Worker-Secret header

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "fallback") {
		t.Errorf("expected direct DNS routing to reach the tunnel, got fallback; body=%q", w.Body.String())
	}
}

func TestSubdomainMiddlewareCFDisabledIgnoresWorkerSecret(t *testing.T) {
	// When CFWorkerEnabled=false, WorkerSecret header should be ignored even if present.
	// Falls through to direct DNS routing.
	cfg := Config{BaseDomain: "tunnel.example.com", WorkerSecret: "testsecret", CFWorkerEnabled: false}
	handler, hub := testSubdomainHandler(cfg)
	hub.Register(closedTunnel("myapp"))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "tunnel.example.com" // base domain, not a subdomain
	req.Header.Set("X-Worker-Secret", "testsecret")
	req.Header.Set("X-Forwarded-Tunnel", "myapp")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 fallback when CFWorkerEnabled=false even with correct secret, got %d", w.Code)
	}
}

func TestSingleTunnelModeRejectsSecondConnection(t *testing.T) {
	hub := NewHub()
	hub.Register(closedTunnel("first"))

	// NewAuth() with no TUNNEL_AUTH_TOKENS set = open mode (all tokens valid).
	cfg := Config{SingleTunnelMode: true}
	auth := NewAuth()
	metrics := NewMetrics()
	limiter := NewRateLimiter()

	srv := httptest.NewServer(wsConnectHandler(hub, auth, cfg, metrics, limiter))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ws/connect", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Guard must return 409 Conflict before attempting WS upgrade.
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %d", resp.StatusCode)
	}
}

func TestSingleTunnelModeAllowsFirstConnection(t *testing.T) {
	hub := NewHub() // empty hub — no tunnels yet

	// NewAuth() with no TUNNEL_AUTH_TOKENS set = open mode (all tokens valid).
	cfg := Config{SingleTunnelMode: true}
	auth := NewAuth()
	metrics := NewMetrics()
	limiter := NewRateLimiter()

	srv := httptest.NewServer(wsConnectHandler(hub, auth, cfg, metrics, limiter))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ws/connect", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Guard must NOT trigger for an empty hub. Handler proceeds to websocket.Accept,
	// which returns 400 because this is a plain HTTP GET (not a WS upgrade request).
	// Any response other than 409 means the guard correctly passed.
	if resp.StatusCode == http.StatusConflict {
		t.Error("single-tunnel guard must not fire when hub is empty")
	}
}

func TestWSConnectRejectsInvalidMode(t *testing.T) {
	hub := NewHub()
	cfg := Config{}
	auth := NewAuth()
	metrics := NewMetrics()
	limiter := NewRateLimiter()
	defer limiter.Close()

	srv := httptest.NewServer(wsConnectHandler(hub, auth, cfg, metrics, limiter))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ws/connect?mode=ssh")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d", resp.StatusCode)
	}
}

func TestWaitResponseTimeoutSendsCancel(t *testing.T) {
	oldTimeout := cachedResponseTimeout
	cachedResponseTimeout = 10 * time.Millisecond
	t.Cleanup(func() { cachedResponseTimeout = oldTimeout })

	serverConn := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	clientConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.CloseNow()

	var conn *websocket.Conn
	select {
	case conn = <-serverConn:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer conn.CloseNow()

	tunnel := NewTunnel("test", "http", conn)
	_, _, err = tunnel.waitResponse(make(chan pendingResponse), "req-timeout")
	if err == nil || !strings.Contains(err.Error(), "response timeout") {
		t.Fatalf("expected response timeout, got %v", err)
	}

	env, _, err := protocol.ReadEnvelope(ctx, clientConn)
	if err != nil {
		t.Fatalf("read cancel envelope: %v", err)
	}
	if env.Type != protocol.TypeHTTPStreamCancel || env.HTTPStreamCancel == nil || env.HTTPStreamCancel.RequestID != "req-timeout" {
		t.Fatalf("unexpected cancel envelope: %#v", env)
	}
}
