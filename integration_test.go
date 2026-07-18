package tunnel_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anivaryam/tunnel/internal/client"
	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/server"
	"github.com/coder/websocket"
)

// waitForTunnel polls the relay's hub until the given tunnel ID is registered
// or the deadline expires. Replaces fixed time.Sleep guesses around the small
// window between assignment-write and hub.Register inside wsConnectHandler.
func waitForTunnel(t *testing.T, hub *server.Hub, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Get(id) != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("tunnel %s did not register within 2s", id)
}

// waitForGone polls until the given tunnel ID is no longer registered,
// covering the small window between the websocket close and Unregister.
func waitForGone(t *testing.T, hub *server.Hub, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Get(id) == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("tunnel %s still registered after 2s", id)
}

func TestIntegration(t *testing.T) {
	// 1. Start a mock local HTTP server.
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			w.Header().Set("X-Custom", "test-value")
			w.WriteHeader(200)
			fmt.Fprint(w, "Hello, World!")
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
			w.WriteHeader(200)
			w.Write(body)
		case "/status":
			w.WriteHeader(404)
			fmt.Fprint(w, "not found")
		default:
			w.WriteHeader(200)
			fmt.Fprintf(w, "path=%s query=%s method=%s", r.URL.Path, r.URL.RawQuery, r.Method)
		}
	}))
	defer local.Close()

	// Extract port from local server URL.
	localPort := strings.TrimPrefix(local.URL, "http://127.0.0.1:")
	localPort = strings.TrimPrefix(localPort, "http://localhost:")
	var portInt int
	fmt.Sscanf(localPort, "%d", &portInt)

	// 2. Start the relay server.
	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relayCfg := server.Config{Port: "0"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	// 3. Connect a tunnel client.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tunnelID, stopClient := connectHTTPTunnel(ctx, t, relay.URL, portInt)
	defer stopClient()
	t.Logf("tunnel ID: %s", tunnelID)

	waitForTunnel(t, relaySrv.Hub, tunnelID)

	baseURL := relay.URL + "/t/" + tunnelID

	// Test 1: Simple GET.
	t.Run("GET /hello", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/hello")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "Hello, World!" {
			t.Errorf("body: got %q, want %q", body, "Hello, World!")
		}
		if resp.Header.Get("X-Custom") != "test-value" {
			t.Errorf("header X-Custom: got %q, want %q", resp.Header.Get("X-Custom"), "test-value")
		}
	})

	// Test 2: POST with body.
	t.Run("POST /echo", func(t *testing.T) {
		resp, err := http.Post(baseURL+"/echo", "application/json", strings.NewReader(`{"key":"value"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != `{"key":"value"}` {
			t.Errorf("body: got %q", body)
		}
	})

	// Test 3: 404 response.
	t.Run("GET /status (404)", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/status")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("status: got %d, want 404", resp.StatusCode)
		}
	})

	// Test 4: Query string and path passthrough.
	t.Run("GET with query", func(t *testing.T) {
		resp, err := http.Get(baseURL + "/api/data?foo=bar&n=1")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := string(body)
		if !strings.Contains(got, "path=/api/data") {
			t.Errorf("expected path=/api/data in %q", got)
		}
		if !strings.Contains(got, "foo=bar&n=1") {
			t.Errorf("expected query foo=bar&n=1 in %q", got)
		}
	})

	// Test 5: Concurrent requests.
	t.Run("concurrent requests", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp, err := http.Get(fmt.Sprintf("%s/concurrent?i=%d", baseURL, i))
				if err != nil {
					t.Errorf("concurrent GET %d: %v", i, err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Errorf("concurrent GET %d: status %d", i, resp.StatusCode)
				}
			}(i)
		}
		wg.Wait()
	})

	// Test 6: Non-existent tunnel returns 502.
	t.Run("non-existent tunnel", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/nonexist/path")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Errorf("status: got %d, want 502", resp.StatusCode)
		}
	})
}

func TestTunnelIDReservation(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token,other-token")
	relayCfg := server.Config{Port: "0"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First connection — get a tunnel ID.
	wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token"
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn1.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment1, err := protocol.ReadEnvelopeOnly(ctx, conn1)
	if err != nil {
		t.Fatalf("read assignment: %v", err)
	}
	tunnelID := assignment1.Assignment.TunnelID
	t.Logf("first connection tunnel ID: %s", tunnelID)

	// Disconnect.
	conn1.Close(websocket.StatusNormalClosure, "bye")
	waitForGone(t, relaySrv.Hub, tunnelID)

	// Reconnect with the same token and tunnel ID — should reclaim.
	wsURL2 := wsURL + "&tunnel_id=" + tunnelID
	conn2, _, err := websocket.Dial(ctx, wsURL2, nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.CloseNow()
	conn2.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment2, err := protocol.ReadEnvelopeOnly(ctx, conn2)
	if err != nil {
		t.Fatalf("read assignment on reconnect: %v", err)
	}

	if assignment2.Assignment.TunnelID != tunnelID {
		t.Errorf("tunnel ID changed: got %q, want %q", assignment2.Assignment.TunnelID, tunnelID)
	}
	t.Logf("reconnected with same tunnel ID: %s", assignment2.Assignment.TunnelID)

	// Disconnect again, then try with a different token — should NOT reclaim.
	conn2.Close(websocket.StatusNormalClosure, "bye")
	waitForGone(t, relaySrv.Hub, tunnelID)

	wsURL3 := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=other-token&tunnel_id=" + tunnelID
	conn3, _, err := websocket.Dial(ctx, wsURL3, nil)
	if err != nil {
		t.Fatalf("dial with other token: %v", err)
	}
	defer conn3.CloseNow()
	conn3.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment3, err := protocol.ReadEnvelopeOnly(ctx, conn3)
	if err != nil {
		t.Fatalf("read assignment: %v", err)
	}

	if assignment3.Assignment.TunnelID == tunnelID {
		t.Error("wrong token should not reclaim the tunnel ID")
	}
	t.Logf("different token got new ID: %s", assignment3.Assignment.TunnelID)
}

func TestAuthRejectsInvalidToken(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "valid-token")
	relayCfg := server.Config{Port: "0"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=bad-token"
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestHealthEndpoint(t *testing.T) {
	relayCfg := server.Config{Port: "0"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	resp, err := http.Get(relay.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body: got %q, want %q", body, "ok")
	}
}

// connectHTTPTunnel runs a real HTTP tunnel client and returns the tunnel ID.
// The caller should defer the returned cancel function.
func connectHTTPTunnel(ctx context.Context, t *testing.T, relayURL string, localPort int) (string, context.CancelFunc) {
	t.Helper()
	clientCtx, cancel := context.WithCancel(ctx)
	c := client.NewClient("ws"+strings.TrimPrefix(relayURL, "http"), "test-token", localPort)
	go func() {
		if err := c.Run(clientCtx); err != nil && clientCtx.Err() == nil {
			t.Logf("client run: %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if id := c.TunnelID(); id != "" {
			return id, cancel
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	t.Fatal("client did not connect within 2s")
	return "", func() {}
}

func TestMultipleHTTPTunnels(t *testing.T) {
	// Start two local servers with distinct responses.
	local1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "server-1")
	}))
	defer local1.Close()

	local2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "server-2")
	}))
	defer local2.Close()

	var port1, port2 int
	fmt.Sscanf(strings.TrimPrefix(local1.URL, "http://127.0.0.1:"), "%d", &port1)
	fmt.Sscanf(strings.TrimPrefix(local2.URL, "http://127.0.0.1:"), "%d", &port2)

	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relaySrv := server.New(server.Config{Port: "0"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id1, stop1 := connectHTTPTunnel(ctx, t, relay.URL, port1)
	defer stop1()

	id2, stop2 := connectHTTPTunnel(ctx, t, relay.URL, port2)
	defer stop2()

	t.Logf("tunnel 1: %s → port %d", id1, port1)
	t.Logf("tunnel 2: %s → port %d", id2, port2)

	waitForTunnel(t, relaySrv.Hub, id1)
	waitForTunnel(t, relaySrv.Hub, id2)

	t.Run("tunnel 1 gets server-1", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + id1 + "/")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "server-1" {
			t.Errorf("got %q, want %q", body, "server-1")
		}
	})

	t.Run("tunnel 2 gets server-2", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + id2 + "/")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "server-2" {
			t.Errorf("got %q, want %q", body, "server-2")
		}
	})

	t.Run("concurrent to both tunnels", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var url, want string
				if i%2 == 0 {
					url = relay.URL + "/t/" + id1 + "/"
					want = "server-1"
				} else {
					url = relay.URL + "/t/" + id2 + "/"
					want = "server-2"
				}
				resp, err := http.Get(url)
				if err != nil {
					t.Errorf("GET %d: %v", i, err)
					return
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if string(body) != want {
					t.Errorf("request %d: got %q, want %q", i, body, want)
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestSubdomainRouting(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer local.Close()
	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relaySrv := server.New(server.Config{Port: "0", BaseDomain: "test.local"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tunnelID, stopClient := connectHTTPTunnel(ctx, t, relay.URL, localPort)
	defer stopClient()
	t.Logf("tunnel ID: %s", tunnelID)

	waitForTunnel(t, relaySrv.Hub, tunnelID)

	// Helper to make requests with a custom Host header.
	doGet := func(path, host string) (*http.Response, error) {
		req, err := http.NewRequest("GET", relay.URL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Host = host
		return http.DefaultClient.Do(req)
	}

	t.Run("subdomain routes to tunnel", func(t *testing.T) {
		resp, err := doGet("/hello", tunnelID+".test.local")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "path=/hello" {
			t.Errorf("body: got %q, want %q", body, "path=/hello")
		}
	})

	t.Run("root path via subdomain", func(t *testing.T) {
		resp, err := doGet("/", tunnelID+".test.local")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "path=/" {
			t.Errorf("body: got %q, want %q", body, "path=/")
		}
	})

	t.Run("non-existent tunnel subdomain returns 502", func(t *testing.T) {
		resp, err := doGet("/", "nonexist.test.local")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Errorf("status: got %d, want 502", resp.StatusCode)
		}
	})

	t.Run("control path /health not proxied", func(t *testing.T) {
		resp, err := doGet("/health", tunnelID+".test.local")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "ok" {
			t.Errorf("expected /health from server, got status=%d body=%q", resp.StatusCode, body)
		}
	})

	t.Run("bare domain falls through to mux", func(t *testing.T) {
		resp, err := doGet("/health", "test.local")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "ok" {
			t.Errorf("expected /health from server, got status=%d body=%q", resp.StatusCode, body)
		}
	})

	t.Run("path-based still works alongside subdomain", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/hello")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "path=/hello" {
			t.Errorf("body: got %q, want %q", body, "path=/hello")
		}
	})

	t.Run("assignment URL uses subdomain", func(t *testing.T) {
		// Reconnect to check the assignment URL format.
		wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token"
		conn2, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn2.CloseNow()
		conn2.SetReadLimit(protocol.MaxBodySize + 4096)

		a, err := protocol.ReadEnvelopeOnly(ctx, conn2)
		if err != nil {
			t.Fatalf("read assignment: %v", err)
		}
		expected := "https://" + a.Assignment.TunnelID + ".test.local"
		if a.Assignment.PublicURL != expected {
			t.Errorf("PublicURL: got %q, want %q", a.Assignment.PublicURL, expected)
		}
	})
}

func TestUDPTunnel(t *testing.T) {
	// 1. Start a local UDP echo server.
	udpEchoAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	udpEcho, err := net.ListenUDP("udp", udpEchoAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer udpEcho.Close()
	echoPort := udpEcho.LocalAddr().(*net.UDPAddr).Port

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			udpEcho.WriteToUDP(buf[:n], addr)
		}
	}()

	// 2. Start relay server.
	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relayCfg := server.Config{Port: "0", PublicHost: "127.0.0.1"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	// 3. Connect a tunnel client in UDP mode.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token&mode=udp"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment, err := protocol.ReadEnvelopeOnly(ctx, conn)
	if err != nil {
		t.Fatalf("read assignment: %v", err)
	}
	udpAddr := assignment.Assignment.UDPAddr
	t.Logf("UDP tunnel ID: %s, addr: %s", assignment.Assignment.TunnelID, udpAddr)

	cw := protocol.NewConnWriter(conn)
	streams := make(map[string]net.Conn)
	var streamsMu sync.Mutex

	// Client loop: handle stream messages.
	go func() {
		for {
			env, body, err := protocol.ReadEnvelope(ctx, conn)
			if err != nil {
				return
			}

			switch env.Type {
			case protocol.TypeStreamOpen:
				go func() {
					localConn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", echoPort))
					if err != nil {
						ack := protocol.Envelope{
							Type:          protocol.TypeStreamOpenAck,
							StreamOpenAck: &protocol.StreamOpenAckPayload{StreamID: env.StreamOpen.StreamID, OK: false, Error: err.Error()},
						}
						cw.WriteEnvelope(ctx, ack, nil)
						return
					}
					streamsMu.Lock()
					streams[env.StreamOpen.StreamID] = localConn
					streamsMu.Unlock()

					ack := protocol.Envelope{
						Type:          protocol.TypeStreamOpenAck,
						StreamOpenAck: &protocol.StreamOpenAckPayload{StreamID: env.StreamOpen.StreamID, OK: true},
					}
					cw.WriteEnvelope(ctx, ack, nil)

					// Local UDP → WS
					go func() {
						buf := make([]byte, 4096)
						for {
							n, err := localConn.Read(buf)
							if n > 0 {
								dataEnv := protocol.Envelope{
									Type:       protocol.TypeStreamData,
									StreamData: &protocol.StreamDataPayload{StreamID: env.StreamOpen.StreamID},
								}
								cw.WriteEnvelope(ctx, dataEnv, buf[:n])
							}
							if err != nil {
								return
							}
						}
					}()
				}()

			case protocol.TypeStreamData:
				streamsMu.Lock()
				lc := streams[env.StreamData.StreamID]
				streamsMu.Unlock()
				if lc != nil {
					lc.Write(body)
				}

			case protocol.TypeStreamClose:
				streamsMu.Lock()
				lc := streams[env.StreamClose.StreamID]
				delete(streams, env.StreamClose.StreamID)
				streamsMu.Unlock()
				if lc != nil {
					lc.Close()
				}
			}
		}
	}()

	waitForTunnel(t, relaySrv.Hub, assignment.Assignment.TunnelID)

	t.Run("UDP echo", func(t *testing.T) {
		resolved, err := net.ResolveUDPAddr("udp", udpAddr)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		udpConn, err := net.DialUDP("udp", nil, resolved)
		if err != nil {
			t.Fatalf("dial udp proxy: %v", err)
		}
		defer udpConn.Close()

		msg := "hello UDP tunnel!"
		_, err = udpConn.Write([]byte(msg))
		if err != nil {
			t.Fatalf("write: %v", err)
		}

		buf := make([]byte, 1024)
		udpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := udpConn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf[:n]) != msg {
			t.Errorf("echo: got %q, want %q", buf[:n], msg)
		}
	})

	t.Run("multiple UDP packets", func(t *testing.T) {
		resolved, err := net.ResolveUDPAddr("udp", udpAddr)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		udpConn, err := net.DialUDP("udp", nil, resolved)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer udpConn.Close()

		for i := 0; i < 5; i++ {
			msg := fmt.Sprintf("pkt-%d", i)
			udpConn.Write([]byte(msg))

			buf := make([]byte, 1024)
			udpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := udpConn.Read(buf)
			if err != nil {
				t.Fatalf("read pkt %d: %v", i, err)
			}
			if string(buf[:n]) != msg {
				t.Errorf("pkt %d: got %q, want %q", i, buf[:n], msg)
			}
		}
	})
}

func TestRefererFallback(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer local.Close()
	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relaySrv := server.New(server.Config{Port: "0"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tunnelID, stopClient := connectHTTPTunnel(ctx, t, relay.URL, localPort)
	defer stopClient()
	waitForTunnel(t, relaySrv.Hub, tunnelID)

	t.Run("absolute path with Referer redirects to tunnel", func(t *testing.T) {
		// Simulate an asset request with an absolute path and a Referer
		// pointing to the tunnel. The fallback handler should 307 redirect
		// to /t/{id}/asset.js.
		httpClient := &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse // don't follow redirects
			},
		}
		req, _ := http.NewRequest("GET", relay.URL+"/asset.js", nil)
		req.Header.Set("Referer", relay.URL+"/t/"+tunnelID+"/index.html")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 307 {
			t.Fatalf("status: got %d, want 307", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		want := "/t/" + tunnelID + "/asset.js"
		if loc != want {
			t.Errorf("Location: got %q, want %q", loc, want)
		}
	})

	t.Run("absolute path without Referer returns tunnel list page", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/asset.js")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "Active Tunnels") {
			t.Error("expected tunnel list page in response")
		}
	})

	t.Run("following redirect serves correct content", func(t *testing.T) {
		// Default client follows redirects, so the full flow works:
		// GET /asset.js (with Referer) → 307 → GET /t/{id}/asset.js → proxied
		req, _ := http.NewRequest("GET", relay.URL+"/asset.js", nil)
		req.Header.Set("Referer", relay.URL+"/t/"+tunnelID+"/index.html")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "path=/asset.js" {
			t.Errorf("body: got %q, want %q", body, "path=/asset.js")
		}
	})
}

func TestWSRewriteInjection(t *testing.T) {
	// Local server that returns HTML and JSON responses.
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Test</title></head><body>hello</body></html>`)
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case "/minimal":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<div>no head tag</div>`)
		default:
			w.WriteHeader(200)
		}
	}))
	defer local.Close()
	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relaySrv := server.New(server.Config{Port: "0"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tunnelID, stopClient := connectHTTPTunnel(ctx, t, relay.URL, localPort)
	defer stopClient()
	waitForTunnel(t, relaySrv.Hub, tunnelID)

	t.Run("HTML response contains WS rewrite script", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/page")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		s := string(body)

		if !strings.Contains(s, "/* tunnel-rewrite */") {
			t.Error("expected tunnel-rewrite script in HTML response")
		}
		if !strings.Contains(s, `var t="`+tunnelID+`"`) {
			t.Errorf("expected tunnel ID %s in injected script", tunnelID)
		}
		// Script should be after <head>
		headIdx := strings.Index(s, "<head>")
		scriptIdx := strings.Index(s, "<script>/* tunnel-rewrite */")
		if headIdx < 0 || scriptIdx < 0 || scriptIdx < headIdx {
			t.Error("script should be injected after <head>")
		}
		// Should contain fetch and XHR patches
		if !strings.Contains(s, "window.fetch=function") {
			t.Error("expected fetch monkey-patch in injected script")
		}
		if !strings.Contains(s, "XMLHttpRequest.prototype.open") {
			t.Error("expected XHR monkey-patch in injected script")
		}
		// Should rewrite hardcoded localhost URLs (e.g. http://localhost:8080/api/...)
		if !strings.Contains(s, `a.hostname==="localhost"`) {
			t.Error("expected localhost rewriting in injected script")
		}
		// Should preserve ws:/wss: protocol for localhost WebSocket URLs
		if !strings.Contains(s, `a.protocol==="ws:"||a.protocol==="wss:"`) {
			t.Error("expected WebSocket protocol preservation for localhost URLs in injected script")
		}
		// Should match on hostname only so tunnelHost:8080 URLs are also rewritten
		if !strings.Contains(s, `a.hostname===window.location.hostname`) {
			t.Error("expected hostname-only matching (not host+port) in injected script")
		}
		// Should use a.hostname + a.port to explicitly clear the port
		if !strings.Contains(s, `a.hostname=window.location.hostname`) {
			t.Error("expected explicit hostname+port assignment to clear port in injected script")
		}
		// Should patch EventSource for SSE streaming
		if !strings.Contains(s, "window.EventSource") {
			t.Error("expected EventSource patch in injected script")
		}
	})

	t.Run("non-HTML response has no injection", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/api")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "tunnel-rewrite") {
			t.Error("JSON response should not contain injected script")
		}
		if string(body) != `{"ok":true}` {
			t.Errorf("JSON body: got %q", body)
		}
	})

	t.Run("HTML without head tag still gets injection", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/minimal")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "/* tunnel-rewrite */") {
			t.Error("expected script injected even without <head> tag")
		}
	})

	t.Run("scoped cookie set by server", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/page")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()

		var hasScoped bool
		for _, c := range resp.Cookies() {
			if c.Name == "__tunnel_id" && c.Value == tunnelID && c.Path == "/t/"+tunnelID+"/" {
				hasScoped = true
			}
		}
		if !hasScoped {
			t.Error("missing scoped cookie with Path=/t/{id}/")
		}
	})

	t.Run("script does not set root cookie via JS", func(t *testing.T) {
		resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/page")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		s := string(body)
		if strings.Contains(s, `document.cookie`) {
			t.Error("rewrite script must not set document.cookie — root-path cookie causes cross-tunnel contamination")
		}
		if strings.Contains(s, "visibilitychange") {
			t.Error("rewrite script must not register visibilitychange listener")
		}
	})
}

func TestSubdomainNoInjection(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html><html><head></head><body>hello</body></html>`)
	}))
	defer local.Close()
	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relaySrv := server.New(server.Config{Port: "0", BaseDomain: "test.local"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tunnelID, stopClient := connectHTTPTunnel(ctx, t, relay.URL, localPort)
	defer stopClient()
	waitForTunnel(t, relaySrv.Hub, tunnelID)

	t.Run("subdomain HTML has no WS rewrite injection", func(t *testing.T) {
		req, _ := http.NewRequest("GET", relay.URL+"/", nil)
		req.Host = tunnelID + ".test.local"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "tunnel-rewrite") {
			t.Error("subdomain routing should NOT inject WS rewrite script")
		}
	})
}

func TestTCPTunnel(t *testing.T) {
	// 1. Start a local TCP echo server.
	tcpEcho, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer tcpEcho.Close()
	echoPort := tcpEcho.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := tcpEcho.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) // echo
			}(conn)
		}
	}()

	// 2. Start relay server (no global TCP proxy needed — per-tunnel ports).
	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")
	relayCfg := server.Config{Port: "0", PublicHost: "127.0.0.1"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	// 3. Connect a tunnel client in TCP mode.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token&mode=tcp"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment, err := protocol.ReadEnvelopeOnly(ctx, conn)
	if err != nil {
		t.Fatalf("read assignment: %v", err)
	}
	tcpAddr := assignment.Assignment.TCPAddr
	t.Logf("TCP tunnel ID: %s, addr: %s", assignment.Assignment.TunnelID, tcpAddr)

	cw := protocol.NewConnWriter(conn)
	streams := make(map[string]net.Conn)
	var streamsMu sync.Mutex

	// Client loop: handle stream messages.
	go func() {
		for {
			env, body, err := protocol.ReadEnvelope(ctx, conn)
			if err != nil {
				return
			}

			switch env.Type {
			case protocol.TypeStreamOpen:
				go func() {
					localConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
					if err != nil {
						ack := protocol.Envelope{
							Type:          protocol.TypeStreamOpenAck,
							StreamOpenAck: &protocol.StreamOpenAckPayload{StreamID: env.StreamOpen.StreamID, OK: false, Error: err.Error()},
						}
						cw.WriteEnvelope(ctx, ack, nil)
						return
					}
					streamsMu.Lock()
					streams[env.StreamOpen.StreamID] = localConn
					streamsMu.Unlock()

					ack := protocol.Envelope{
						Type:          protocol.TypeStreamOpenAck,
						StreamOpenAck: &protocol.StreamOpenAckPayload{StreamID: env.StreamOpen.StreamID, OK: true},
					}
					cw.WriteEnvelope(ctx, ack, nil)

					// Local → WS
					go func() {
						buf := make([]byte, 4096)
						for {
							n, err := localConn.Read(buf)
							if n > 0 {
								dataEnv := protocol.Envelope{
									Type:       protocol.TypeStreamData,
									StreamData: &protocol.StreamDataPayload{StreamID: env.StreamOpen.StreamID},
								}
								cw.WriteEnvelope(ctx, dataEnv, buf[:n])
							}
							if err != nil {
								closeEnv := protocol.Envelope{
									Type:        protocol.TypeStreamClose,
									StreamClose: &protocol.StreamClosePayload{StreamID: env.StreamOpen.StreamID},
								}
								cw.WriteEnvelope(ctx, closeEnv, nil)
								return
							}
						}
					}()
				}()

			case protocol.TypeStreamData:
				streamsMu.Lock()
				lc := streams[env.StreamData.StreamID]
				streamsMu.Unlock()
				if lc != nil {
					lc.Write(body)
				}

			case protocol.TypeStreamClose:
				streamsMu.Lock()
				lc := streams[env.StreamClose.StreamID]
				delete(streams, env.StreamClose.StreamID)
				streamsMu.Unlock()
				if lc != nil {
					lc.Close()
				}
			}
		}
	}()

	waitForTunnel(t, relaySrv.Hub, assignment.Assignment.TunnelID)

	// 4. Connect via the per-tunnel TCP proxy and test echo.
	t.Run("TCP echo", func(t *testing.T) {
		tcpConn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial tcp proxy: %v", err)
		}
		defer tcpConn.Close()

		msg := "hello TCP tunnel!"
		_, err = tcpConn.Write([]byte(msg))
		if err != nil {
			t.Fatalf("write: %v", err)
		}

		buf := make([]byte, 1024)
		tcpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := tcpConn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got := string(buf[:n])
		if got != msg {
			t.Errorf("echo: got %q, want %q", got, msg)
		}
	})

	// 5. Concurrent TCP streams.
	t.Run("concurrent TCP streams", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tcpConn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second)
				if err != nil {
					t.Errorf("dial %d: %v", i, err)
					return
				}
				defer tcpConn.Close()

				msg := fmt.Sprintf("msg-%d", i)
				tcpConn.Write([]byte(msg))

				buf := make([]byte, 1024)
				tcpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := tcpConn.Read(buf)
				if err != nil {
					t.Errorf("read %d: %v", i, err)
					return
				}
				if string(buf[:n]) != msg {
					t.Errorf("echo %d: got %q, want %q", i, buf[:n], msg)
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestIntegrationNamedTunnel(t *testing.T) {
	t.Setenv("TUNNEL_AUTH_TOKENS", "test-token")

	relayCfg := server.Config{Port: "0"}
	relaySrv := server.New(relayCfg)
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First client claims "eservice".
	wsURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token&name=eservice"
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer conn1.CloseNow()
	conn1.SetReadLimit(protocol.MaxBodySize + 4096)

	assignment, err := protocol.ReadEnvelopeOnly(ctx, conn1)
	if err != nil {
		t.Fatalf("read assignment 1: %v", err)
	}
	if assignment.Assignment == nil || assignment.Assignment.TunnelID != "eservice" {
		t.Fatalf("expected tunnel ID 'eservice', got %+v", assignment.Assignment)
	}

	// Second client tries to claim the same name and should be rejected.
	// A dial-time failure is also a valid rejection (server may close during handshake).
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		conn2.SetReadLimit(protocol.MaxBodySize + 4096)
		_, readErr := protocol.ReadEnvelopeOnly(ctx, conn2)
		if readErr == nil {
			t.Fatal("expected second claim of 'eservice' to be rejected, but got assignment")
		}
		if code := websocket.CloseStatus(readErr); code != websocket.StatusPolicyViolation {
			t.Errorf("expected StatusPolicyViolation close code for second-claim rejection, got %v", code)
		}
		conn2.CloseNow()
	}

	// Invalid name (uppercase) should be rejected.
	// A dial-time failure is also a valid rejection (server may close during handshake).
	badURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token&name=ESERVICE"
	conn3, _, err := websocket.Dial(ctx, badURL, nil)
	if err == nil {
		conn3.SetReadLimit(protocol.MaxBodySize + 4096)
		_, readErr := protocol.ReadEnvelopeOnly(ctx, conn3)
		if readErr == nil {
			t.Fatal("expected uppercase name to be rejected")
		}
		if code := websocket.CloseStatus(readErr); code != websocket.StatusPolicyViolation {
			t.Errorf("expected StatusPolicyViolation close code for uppercase-name rejection, got %v", code)
		}
		conn3.CloseNow()
	}

	// Reserved name should be rejected.
	// A dial-time failure is also a valid rejection (server may close during handshake).
	resvURL := "ws" + strings.TrimPrefix(relay.URL, "http") + "/ws/connect?token=test-token&name=dashboard"
	conn4, _, err := websocket.Dial(ctx, resvURL, nil)
	if err == nil {
		conn4.SetReadLimit(protocol.MaxBodySize + 4096)
		_, readErr := protocol.ReadEnvelopeOnly(ctx, conn4)
		if readErr == nil {
			t.Fatal("expected reserved name 'dashboard' to be rejected")
		}
		if code := websocket.CloseStatus(readErr); code != websocket.StatusPolicyViolation {
			t.Errorf("expected StatusPolicyViolation close code for reserved-name rejection, got %v", code)
		}
		conn4.CloseNow()
	}

	// A different token can reclaim the name only after the holder fully
	// disconnects AND the 60s reservation window expires. We don't wait 60s
	// in tests; instead, verify the in-use rejection above is sufficient.
}

func TestSSEStreaming(t *testing.T) {
	// Local server that serves an SSE stream of three events then closes.
	events := []string{
		"data: event-1\n\n",
		"data: event-2\n\n",
		"data: event-3\n\n",
	}

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}
		for _, e := range events {
			fmt.Fprint(w, e)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer local.Close()

	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	relaySrv := server.New(server.Config{Port: "0"})
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a real Client (not a manual websocket loop) so the SSE
	// streaming path in handleRequest is exercised.
	c := client.NewClient("ws"+strings.TrimPrefix(relay.URL, "http"), "", localPort)
	go c.Run(ctx)

	// Wait for the client to connect and get its tunnel ID.
	var tunnelID string
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		if id := c.TunnelID(); id != "" {
			tunnelID = id
			break
		}
	}
	if tunnelID == "" {
		t.Fatal("client did not connect in time")
	}

	resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type: got %q, want text/event-stream", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	got := string(body)
	for _, e := range events {
		if !strings.Contains(got, strings.TrimSpace(e)) {
			t.Errorf("missing event %q in response:\n%s", e, got)
		}
	}
}

func TestLargeDownload(t *testing.T) {
	const largeSize = 2 * 1024 * 1024

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/large" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		data := make([]byte, largeSize)
		for i := range data {
			data[i] = byte(i % 256)
		}
		w.Write(data)
	}))
	defer local.Close()

	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	relaySrv := server.New(server.Config{Port: "0"})
	defer relaySrv.Close()
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := client.NewClient("ws"+strings.TrimPrefix(relay.URL, "http"), "", localPort)
	go c.Run(ctx)

	var tunnelID string
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		if id := c.TunnelID(); id != "" {
			tunnelID = id
			break
		}
	}
	if tunnelID == "" {
		t.Fatal("client did not connect in time")
	}

	resp, err := http.Get(relay.URL + "/t/" + tunnelID + "/large")
	if err != nil {
		t.Fatalf("GET /large: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if len(body) != largeSize {
		t.Errorf("body length: got %d, want %d", len(body), largeSize)
	}
}

func TestMixedHTTPAndStreamTraffic(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			w.WriteHeader(200)
			fmt.Fprint(w, "Hello")
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", 500)
				return
			}
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "data: event-%d\n\n", i)
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer local.Close()

	var localPort int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &localPort)

	relaySrv := server.New(server.Config{Port: "0"})
	defer relaySrv.Close()
	relay := httptest.NewServer(relaySrv.Handler)
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := client.NewClient("ws"+strings.TrimPrefix(relay.URL, "http"), "", localPort)
	go c.Run(ctx)

	var tunnelID string
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		if id := c.TunnelID(); id != "" {
			tunnelID = id
			break
		}
	}
	if tunnelID == "" {
		t.Fatal("client did not connect in time")
	}

	baseURL := relay.URL + "/t/" + tunnelID

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		resp, err := http.Get(baseURL + "/hello")
		if err != nil {
			t.Errorf("GET /hello: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("/hello status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "Hello" {
			t.Errorf("/hello body: got %q, want Hello", body)
		}
	}()

	go func() {
		defer wg.Done()
		resp, err := http.Get(baseURL + "/hello")
		if err != nil {
			t.Errorf("GET /hello (2): %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("/hello (2) status: got %d, want 200", resp.StatusCode)
		}
	}()

	go func() {
		defer wg.Done()
		resp, err := http.Get(baseURL + "/events")
		if err != nil {
			t.Errorf("GET /events: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("/events status: got %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		for i := 0; i < 5; i++ {
			if !strings.Contains(string(body), fmt.Sprintf("event-%d", i)) {
				t.Errorf("/events missing event-%d", i)
			}
		}
	}()

	wg.Wait()
}
