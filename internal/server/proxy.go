package server

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// tunnelRewriteScriptTemplate is injected into HTML responses served via
// path-based routing. It rewrites same-host and localhost WebSocket, fetch,
// and XHR requests to go through /t/{id}/, and strips the tunnel prefix from
// the browser URL so SPA routers (React Router, Vue Router, etc.) see clean paths.
// rootHandler uses the Referer header to recover the tunnel ID for subsequent
// resource loads; the path-scoped __tunnel_id cookie (set server-side) is a fallback.
//
// What it patches:
//   - history.replaceState — strips /t/{id}/ prefix so SPA routers match routes
//   - window.WebSocket     — for Vite HMR and dev-server WebSockets
//   - window.fetch         — for API calls and background requests (including hardcoded localhost URLs)
//   - XMLHttpRequest.open  — for legacy AJAX (including hardcoded localhost URLs)
//
// Port normalisation: rw() matches on hostname only so it also catches URLs
// like window.location.hostname+':8080' — a common pattern when the app
// constructs API base URLs from the current hostname and appends the local
// backend port. The explicit port is cleared so the request goes through the
// standard tunnel HTTPS port instead of the unreachable internal port (which
// would leave CORS preflights pending forever).
//
// EventSource is also patched so SSE streams (e.g. /api/notifications/stream)
// are routed through the tunnel instead of hitting localhost directly and
// triggering a CORS error.
const tunnelRewriteScriptTemplate = `<script>/* tunnel-rewrite */(function(){var t="TUNNEL_ID_PLACEHOLDER";var p="/t/"+t+"/";var l=window.location;if(l.pathname.startsWith(p)){history.replaceState(null,"","/"+l.pathname.slice(p.length)+l.search+l.hash)}function rw(u){try{var a=new URL(u,window.location.href);var L=a.hostname==="localhost"||a.hostname==="127.0.0.1";var sH=a.hostname===window.location.hostname;if((L||sH)&&!a.pathname.startsWith(p)){if(L||a.port!==""){var ws=a.protocol==="ws:"||a.protocol==="wss:";a.protocol=ws?(window.location.protocol==="https:"?"wss:":"ws:"):window.location.protocol;a.hostname=window.location.hostname;a.port=window.location.port}a.pathname=p+a.pathname.replace(/^\//,"");return a.toString()}}catch(e){}return u}var OWS=window.WebSocket;window.WebSocket=function(u,r){u=rw(u);if(r!==undefined)return new OWS(u,r);return new OWS(u)};window.WebSocket.prototype=OWS.prototype;window.WebSocket.CONNECTING=OWS.CONNECTING;window.WebSocket.OPEN=OWS.OPEN;window.WebSocket.CLOSING=OWS.CLOSING;window.WebSocket.CLOSED=OWS.CLOSED;var OF=window.fetch;window.fetch=function(i,n){if(typeof i==="string"){i=rw(i)}else if(i instanceof Request){var nu=rw(i.url);if(nu!==i.url)i=new Request(nu,i)}return OF.call(this,i,n)};var OX=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(){if(typeof arguments[1]==="string"){arguments[1]=rw(arguments[1])}return OX.apply(this,arguments)};var OE=window.EventSource;if(OE){window.EventSource=function(u,i){u=rw(u);return i!==undefined?new OE(u,i):new OE(u)};window.EventSource.prototype=OE.prototype;window.EventSource.CONNECTING=OE.CONNECTING;window.EventSource.OPEN=OE.OPEN;window.EventSource.CLOSED=OE.CLOSED}})()</script>`

// ProxyHandler handles incoming HTTP requests and forwards them through the tunnel.
func ProxyHandler(hub *Hub, metrics *Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path format: /t/{tunnelID}/...
		path := r.URL.Path
		if !strings.HasPrefix(path, "/t/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Strip "/t/" prefix and split on next "/"
		rest := path[3:]
		tunnelID, subPath, found := strings.Cut(rest, "/")
		if tunnelID == "" {
			http.Error(w, "missing tunnel ID", http.StatusBadRequest)
			return
		}
		// /t/{id} with no trailing slash: redirect to /t/{id}/ so that relative
		// URLs in served HTML resolve against the correct base path.
		if !found {
			target := "/t/" + tunnelID + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		subPath = "/" + subPath

		tunnel := hub.Get(tunnelID)
		if tunnel == nil {
			http.Error(w, "tunnel not found", http.StatusBadGateway)
			return
		}

		// Path-scoped cookie for single-tunnel fallback when Referer lacks the /t/ prefix
		// (e.g. hard-refresh after SPA URL rewriting). Referer-based detection takes precedence.
		http.SetCookie(w, &http.Cookie{
			Name:     "__tunnel_id",
			Value:    tunnelID,
			Path:     "/t/" + tunnelID + "/",
			SameSite: http.SameSiteLaxMode,
		})
		// Root-scope cookie so rootHandler can recover the tunnel ID after
		// a hard refresh when the browser doesn't send the path-scoped cookie.
		// Subdomain routing never reaches this handler (subdomainMiddleware intercepts
		// first), so this has no effect on that mode.
		http.SetCookie(w, &http.Cookie{
			Name:     "__tunnel_root_id",
			Value:    tunnelID,
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
		})

		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, tunnel, tunnelID, subPath, metrics)
			return
		}

		proxyRequest(w, r, tunnel, tunnelID, subPath, metrics, false)
	}
}

// tunnelIDFromRequest extracts a tunnel ID from the Referer header first
// (preferred — it is tab-specific so multiple tunnels work), then falls
// back to the cookie.
func tunnelIDFromRequest(r *http.Request) string {
	if ref := r.Header.Get("Referer"); ref != "" {
		if idx := strings.Index(ref, "/t/"); idx >= 0 {
			rest := ref[idx+3:]
			id, _, _ := strings.Cut(rest, "/")
			if id != "" {
				return id
			}
		}
	}
	if c, err := r.Cookie("__tunnel_id"); err == nil && c.Value != "" {
		return c.Value
	}
	if c, err := r.Cookie("__tunnel_root_id"); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// proxyRequest forwards an HTTP request through the given tunnel and writes the response.
// When isSubdomain is false (path-based routing), a rewrite script is injected into HTML
// responses that patches WebSocket, fetch, and XHR to route through the tunnel path.
func proxyRequest(w http.ResponseWriter, r *http.Request, tunnel *Tunnel, tunnelID, subPath string, metrics *Metrics, isSubdomain bool) {
	start := time.Now()

	// Filter hop-by-hop headers, then strip and rewrite X-Forwarded-* so the
	// downstream app sees server-observed values rather than peer-supplied ones.
	headers := filterHopByHop(r.Header)
	sanitizeAndFillForwarded(headers, r)

	reqID := uuid.New().String()

	// Stream large or unknown-length request bodies to avoid buffering them fully.
	// ContentLength < 0 means chunked / unknown, but only for methods that actually
	// carry a body (POST, PUT, PATCH). GET/HEAD/OPTIONS etc. have ContentLength == -1
	// with no body and must NOT be treated as streaming uploads.
	streamUpload := r.ContentLength > protocol.RequestStreamThreshold ||
		(r.ContentLength < 0 && requestBodyMethod(r.Method))

	var body []byte
	if !streamUpload {
		var err error
		body, err = protocol.ReadBody(r.Body)
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}

	env := protocol.Envelope{
		Type:      protocol.TypeHTTPRequest,
		RequestID: reqID,
		HTTPRequest: &protocol.HTTPRequestPayload{
			Method:        r.Method,
			Path:          subPath,
			Query:         r.URL.RawQuery,
			Headers:       headers,
			ContentLength: r.ContentLength,
			Streaming:     streamUpload,
		},
	}
	if !streamUpload {
		env.HTTPRequest.ContentLength = int64(len(body))
	}

	ch, err := tunnel.sendRequest(r.Context(), env, body)
	if err != nil {
		log.Printf("[proxy] send request to tunnel %s: %v", tunnelID, err)
		http.Error(w, "tunnel disconnected", http.StatusBadGateway)
		return
	}

	// For streaming uploads, relay the request body as chunks after registering
	// the pending response (so the response can arrive while we're still sending).
	if streamUpload {
		go streamRequestBody(r.Context(), r.Body, reqID, tunnel)
	}

	respEnv, respBody, err := tunnel.waitResponse(ch, reqID)
	if err != nil {
		log.Printf("[proxy] wait response from tunnel %s: %v", tunnelID, err)
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}

	if respEnv.HTTPResponse == nil {
		http.Error(w, "invalid response from tunnel", http.StatusBadGateway)
		return
	}

	// Streaming response (SSE / chunked): write headers immediately and relay
	// chunks as they arrive. The client sends TypeHTTPStreamChunk messages until
	// TypeHTTPStreamEnd (or the browser disconnects, in which case we send
	// TypeHTTPStreamCancel so the client aborts the local connection).
	if respEnv.HTTPResponse.Streaming {
		respHeaders := filterHopByHop(respEnv.HTTPResponse.Headers)
		respHeaders = stripReservedSetCookies(respHeaders)
		// Remove Content-Length — length is unknown for streaming responses.
		delete(respHeaders, "Content-Length")
		for k, vals := range respHeaders {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(respEnv.HTTPResponse.StatusCode)
		flusher, canFlush := w.(http.Flusher)
		if canFlush {
			flusher.Flush()
		}

		sseStream := tunnel.GetSSEStream(reqID)
		defer func() {
			tunnel.RemoveSSEStream(reqID)
			if err := tunnel.Writer.WriteEnvelope(r.Context(), protocol.Envelope{
				Type:             protocol.TypeHTTPStreamCancel,
				HTTPStreamCancel: &protocol.HTTPStreamCancelPayload{RequestID: reqID},
			}, nil); err != nil {
				log.Printf("[tunnel %s] write stream cancel: %v", tunnel.ID, err)
			}
		}()

		if sseStream != nil {
			for {
				select {
				case chunk, ok := <-sseStream:
					if !ok {
						return
					}
					w.Write(chunk)
					if canFlush {
						flusher.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		}
		metrics.OnHTTPRequest(r.Method, fmt.Sprintf("%d", respEnv.HTTPResponse.StatusCode), time.Since(start).Seconds())
		return
	}

	respHeaders := filterHopByHop(respEnv.HTTPResponse.Headers)
	respHeaders = stripReservedSetCookies(respHeaders)

	// Inject tunnel rewrite script into HTML responses served via
	// path-based routing. The script patches WebSocket, fetch, XHR, and
	// EventSource to route through /t/{id}/, and maintains a root cookie
	// for browser resource loads. Subdomain routing doesn't need this.
	if !isSubdomain && isHTMLResponse(respHeaders) && len(respBody) > 0 {
		// Skip injection for compressed responses — the body is opaque.
		if _, compressed := respHeaders["Content-Encoding"]; !compressed {
			respBody = injectTunnelRewriteScript(respBody, tunnelID)
			delete(respHeaders, "Content-Length")
		}
	}

	// Copy response headers.
	for k, vals := range respHeaders {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}

	// Set Content-Length to match the (possibly modified) body.
	if len(respBody) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	}

	w.WriteHeader(respEnv.HTTPResponse.StatusCode)
	if len(respBody) > 0 {
		w.Write(respBody)
	}

	metrics.OnHTTPRequest(r.Method, fmt.Sprintf("%d", respEnv.HTTPResponse.StatusCode), time.Since(start).Seconds())
	metrics.OnBytes("inbound", len(body))
	metrics.OnBytes("outbound", len(respBody))
}

// isWebSocketUpgrade checks whether the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// proxyWebSocket accepts a browser WebSocket and relays frames through the tunnel.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, tunnel *Tunnel, tunnelID, subPath string, _ *Metrics) {
	streamID := uuid.New().String()

	// Forward non-hop-by-hop headers to the client for the local WS dial.
	headers := filterHopByHop(r.Header)

	// Ask the tunnel client to open a WebSocket to the local server.
	ok, err := tunnel.OpenWSStream(r.Context(), streamID, subPath, r.URL.RawQuery, headers)
	if err != nil || !ok {
		log.Printf("[ws-proxy] tunnel %s: open ws stream %s failed: %v", tunnelID, streamID, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	// Accept the browser-side WebSocket.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("[ws-proxy] tunnel %s: accept browser ws: %v", tunnelID, err)
		tunnel.SendWSClose(r.Context(), streamID)
		tunnel.RemoveWSStream(streamID)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxBodySize)

	ws := tunnel.GetWSStream(streamID)
	if ws == nil {
		conn.Close(websocket.StatusInternalError, "stream not found")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Browser → Tunnel: read from browser WS, send ws_frame through tunnel.
	safego.Go("proxy.browser2tunnel", func() {
		defer cancel()
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env := protocol.Envelope{
				Type: protocol.TypeWSFrame,
				WSFrame: &protocol.WSFramePayload{
					StreamID:    streamID,
					MessageType: int(msgType),
				},
			}
			if err := tunnel.Writer.WriteEnvelope(ctx, env, data); err != nil {
				return
			}
		}
	})

	// Tunnel → Browser: read from frame channel, write to browser WS.
	for {
		select {
		case <-ctx.Done():
			tunnel.SendWSClose(ctx, streamID)
			tunnel.RemoveWSStream(streamID)
			return
		case frame, ok := <-ws.FrameCh:
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if err := conn.Write(ctx, websocket.MessageType(frame.Type), frame.Data); err != nil {
				tunnel.SendWSClose(ctx, streamID)
				tunnel.RemoveWSStream(streamID)
				return
			}
		}
	}
}

// reservedCookieNames are cookies the relay sets itself; a compromised local
// app must not be allowed to shadow them via response Set-Cookie. Filtering is
// done by name prefix to cover __tunnel_id, __tunnel_root_id, __tunnel_dashboard.
var reservedCookieNames = []string{"__tunnel_"}

// stripReservedSetCookies removes any Set-Cookie whose cookie name starts with
// one of reservedCookieNames. Returned headers are the filtered set; original
// header map is left untouched.
func stripReservedSetCookies(headers map[string][]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for k, vals := range headers {
		if !strings.EqualFold(k, "Set-Cookie") {
			out[k] = vals
			continue
		}
		var kept []string
		for _, v := range vals {
			cookieName := v
			if i := strings.IndexByte(v, '='); i >= 0 {
				cookieName = strings.TrimSpace(v[:i])
			}
			reserved := false
			for _, p := range reservedCookieNames {
				if strings.HasPrefix(strings.ToLower(cookieName), p) {
					reserved = true
					break
				}
			}
			if !reserved {
				kept = append(kept, v)
			} else {
				log.Printf("[proxy] dropping reserved Set-Cookie %q from local app", cookieName)
			}
		}
		if len(kept) > 0 {
			out[k] = kept
		}
	}
	return out
}

// forwardedSensitiveHeaders are inbound headers a peer must not be allowed to
// dictate; the proxy strips them on entry and sets its own values where
// applicable. Without this, an external client can lie about their source IP
// or scheme and the local app will believe them.
var forwardedSensitiveHeaders = map[string]bool{
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
	"X-Forwarded-Port":  true,
	"X-Real-Ip":         true,
	"Forwarded":         true,
}

func filterHopByHop(h map[string][]string) map[string][]string {
	if h == nil {
		return nil
	}
	connHeader := h["Connection"]
	filtered := make(map[string][]string, len(h))
	for k, v := range h {
		if protocol.IsHopByHop(k, connHeader) {
			continue
		}
		filtered[k] = v
	}
	return filtered
}

// trustProxyHeaders, when true, lets the relay believe an upstream
// X-Forwarded-Proto / X-Forwarded-For header instead of overwriting it.
// Required when this binary sits behind a TLS-terminating LB (Railway,
// Cloudflare, fly.io, etc.); should remain false when the relay is the
// edge. Set via TUNNEL_TRUST_PROXY=true at startup.
var trustProxyHeaders = strings.EqualFold(os.Getenv("TUNNEL_TRUST_PROXY"), "true")

// sanitizeAndFillForwarded strips inbound X-Forwarded-* / Forwarded headers
// (peer-controlled, must not be trusted) and replaces them with values
// observed by this server. The result is the only X-Forwarded-* set the
// downstream local app can rely on. When TUNNEL_TRUST_PROXY=true the
// upstream X-Forwarded-Proto / X-Forwarded-For headers are honoured before
// being stripped, so traffic that arrives via a trusted LB keeps the
// original client IP and scheme.
func sanitizeAndFillForwarded(headers map[string][]string, r *http.Request) {
	upstreamProto := ""
	upstreamFor := ""
	if trustProxyHeaders {
		upstreamProto = r.Header.Get("X-Forwarded-Proto")
		upstreamFor = r.Header.Get("X-Forwarded-For")
	}
	for k := range headers {
		if forwardedSensitiveHeaders[http.CanonicalHeaderKey(k)] {
			delete(headers, k)
		}
	}
	clientIP := r.RemoteAddr
	if h, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = h
	}
	if upstreamFor != "" {
		clientIP = upstreamFor
	}
	headers["X-Forwarded-For"] = []string{clientIP}
	headers["X-Real-Ip"] = []string{clientIP}
	headers["X-Forwarded-Host"] = []string{r.Host}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(upstreamProto, "https") {
		scheme = "https"
	}
	headers["X-Forwarded-Proto"] = []string{scheme}
}

// isHTMLResponse returns true if the response Content-Type is text/html.
func isHTMLResponse(headers map[string][]string) bool {
	for _, v := range headers["Content-Type"] {
		if strings.HasPrefix(strings.ToLower(v), "text/html") {
			return true
		}
	}
	return false
}

// injectScanLimit caps how much of the response body we lowercase to find an
// injection point. <head>, <html>, and <!doctype> all sit in the first few
// hundred bytes of any sane HTML document; lowercasing a multi-megabyte body
// to find them was wasteful.
const injectScanLimit = 8 * 1024

// injectTunnelRewriteScript inserts the tunnel rewrite script right after <head>,
// <html>, or <!doctype>. Falls back to prepending if none are found.
// Defence-in-depth: tunnel IDs are restricted to base62 / nameRegex (no quotes
// or backslashes), but JS-escape anyway so a future ValidName change cannot
// silently introduce XSS.
func injectTunnelRewriteScript(body []byte, tunnelID string) []byte {
	script := []byte(strings.Replace(tunnelRewriteScriptTemplate, "TUNNEL_ID_PLACEHOLDER", template.JSEscapeString(tunnelID), 1))
	scanLen := len(body)
	if scanLen > injectScanLimit {
		scanLen = injectScanLimit
	}
	lower := bytes.ToLower(body[:scanLen])

	// Try to inject after <head...>
	if idx := bytes.Index(lower, []byte("<head")); idx >= 0 {
		if end := bytes.IndexByte(lower[idx:], '>'); end >= 0 {
			at := idx + end + 1
			return spliceBytes(body, at, script)
		}
	}

	// Fallback: after <html...>
	if idx := bytes.Index(lower, []byte("<html")); idx >= 0 {
		if end := bytes.IndexByte(lower[idx:], '>'); end >= 0 {
			at := idx + end + 1
			return spliceBytes(body, at, script)
		}
	}

	// Fallback: after <!doctype...>
	if idx := bytes.Index(lower, []byte("<!doctype")); idx >= 0 {
		if end := bytes.IndexByte(lower[idx:], '>'); end >= 0 {
			at := idx + end + 1
			return spliceBytes(body, at, script)
		}
	}

	// Last resort: prepend.
	return append(script, body...)
}

func spliceBytes(original []byte, at int, insert []byte) []byte {
	result := make([]byte, len(original)+len(insert))
	copy(result, original[:at])
	copy(result[at:], insert)
	copy(result[at+len(insert):], original[at:])
	return result
}

// requestBodyMethod reports whether the HTTP method is one that semantically
// carries a request body (POST, PUT, PATCH). GET, HEAD, OPTIONS, and DELETE
// typically have ContentLength == -1 without a body; streaming them would
// create an unnecessary io.Pipe and stall the local HTTP request.
func requestBodyMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// streamRequestBody streams an HTTP request body to the tunnel client as
// TypeHTTPRequestChunk messages followed by TypeHTTPRequestEnd. It is run in
// a goroutine so the server can begin waiting for the response in parallel.
func streamRequestBody(ctx context.Context, body io.ReadCloser, reqID string, tunnel *Tunnel) {
	defer body.Close()
	buf := make([]byte, protocol.MaxStreamChunk)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunkEnv := protocol.Envelope{
				Type:             protocol.TypeHTTPRequestChunk,
				HTTPRequestChunk: &protocol.HTTPRequestChunkPayload{RequestID: reqID},
			}
			if writeErr := tunnel.Writer.WriteEnvelope(ctx, chunkEnv, buf[:n]); writeErr != nil {
				log.Printf("[proxy] stream upload chunk for %s: %v", reqID, writeErr)
				return
			}
		}
		if err != nil {
			break
		}
	}
	endEnv := protocol.Envelope{
		Type:           protocol.TypeHTTPRequestEnd,
		HTTPRequestEnd: &protocol.HTTPRequestEndPayload{RequestID: reqID},
	}
	if err := tunnel.Writer.WriteEnvelope(ctx, endEnv, nil); err != nil {
		log.Printf("[tunnel %s] write request end: %v", tunnel.ID, err)
	}
}

// rootHandler is registered at "/" and handles four cases:
//  1. Single mode + tunnel active  → proxy directly (rewrite script suppressed)
//  2. Single mode + no tunnel      → 503 "No Tunnel" page
//  3. Multi mode + Referer/cookie  → redirect to /t/{id}/ (SPA fallback, unchanged)
//  4. Multi mode + no context      → 200 tunnel list page
func rootHandler(hub *Hub, metrics *Metrics, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.SingleTunnelMode {
			tunnels := hub.List()
			if len(tunnels) == 0 {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusServiceUnavailable)
				renderNoTunnelPage(w)
				return
			}
			tunnel := tunnels[0]
			if isWebSocketUpgrade(r) {
				proxyWebSocket(w, r, tunnel, tunnel.ID, r.URL.Path, metrics)
				return
			}
			// Pass isSubdomain=true to suppress /t/{id}/ rewrite script injection.
			// In single-tunnel mode the browser URL is already "/" — no prefix to strip.
			proxyRequest(w, r, tunnel, tunnel.ID, r.URL.Path, metrics, true)
			return
		}

		// Multi-tunnel mode: recover tunnel ID from Referer/cookie for SPA navigation.
		if tunnelID := tunnelIDFromRequest(r); tunnelID != "" {
			tunnel := hub.Get(tunnelID)
			if tunnel != nil {
				if isWebSocketUpgrade(r) {
					proxyWebSocket(w, r, tunnel, tunnelID, r.URL.Path, metrics)
					return
				}
				target := "/t/" + tunnelID + r.URL.Path
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				return
			}
		}

		// No context: render tunnel list (or empty state).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		renderTunnelListPage(w, hub.List(), cfg)
	}
}

// publicPageStyles is the shared futuristic-HUD stylesheet used by the
// public no-tunnel and tunnel-list pages. Keep in sync with the admin
// dashboard's visual language but expose no per-client detail.
const publicPageStyles = `
:root {
  --bg-0:#04060b; --bg-1:#080d18; --bg-2:#0c1322;
  --line:rgba(118,230,255,0.12); --line-strong:rgba(118,230,255,0.32);
  --fg:#d6e6ff; --fg-dim:#6a7e9a;
  --accent:#00f0ff; --accent-2:#ff2bd6; --accent-3:#b8ff5e; --warn:#ffb547;
  --shadow-glow:0 0 24px rgba(0,240,255,0.18),0 0 80px rgba(255,43,214,0.06);
}
*{margin:0;padding:0;box-sizing:border-box}
html,body{background:var(--bg-0);color:var(--fg);font-family:'JetBrains Mono',ui-monospace,monospace;min-height:100dvh}
body{
  background:
    radial-gradient(1200px 600px at 80% -10%,rgba(255,43,214,.10),transparent 60%),
    radial-gradient(1000px 500px at -10% 100%,rgba(0,240,255,.10),transparent 60%),
    linear-gradient(180deg,#04060b 0%,#060912 100%);
  overflow-x:hidden;-webkit-font-smoothing:antialiased;
}
#bg{position:fixed;inset:0;z-index:0;pointer-events:none}
#bg canvas{width:100%;height:100%;display:block;opacity:.55}
.scanlines{position:fixed;inset:0;z-index:1;pointer-events:none;mix-blend-mode:overlay;
  background:repeating-linear-gradient(to bottom,rgba(255,255,255,.025) 0 1px,transparent 1px 3px);opacity:.6}
.vignette{position:fixed;inset:0;z-index:1;pointer-events:none;box-shadow:inset 0 0 220px 40px rgba(0,0,0,.65)}
main{position:relative;z-index:2;max-width:1280px;margin:0 auto;padding:24px clamp(16px,4vw,40px) 80px}
.topbar{display:flex;align-items:center;justify-content:space-between;gap:16px;
  padding:14px 18px;margin-bottom:24px;border:1px solid var(--line);border-radius:14px;
  background:linear-gradient(180deg,rgba(8,13,24,.85),rgba(8,13,24,.55));
  backdrop-filter:blur(14px) saturate(140%);-webkit-backdrop-filter:blur(14px) saturate(140%);
  box-shadow:var(--shadow-glow)}
.brand{display:flex;align-items:center;gap:12px;min-width:0}
.logo{width:36px;height:36px;flex:0 0 36px;border:1px solid var(--line-strong);border-radius:10px;
  display:grid;place-items:center;background:radial-gradient(circle at 30% 30%,rgba(0,240,255,.35),transparent 60%);
  box-shadow:inset 0 0 12px rgba(0,240,255,.35)}
.logo svg{width:20px;height:20px}
.brand h1{font-family:'Orbitron',sans-serif;font-weight:900;font-size:15px;letter-spacing:.28em;color:var(--fg);text-transform:uppercase;line-height:1}
.brand small{font-size:10px;letter-spacing:.32em;color:var(--fg-dim);text-transform:uppercase;display:block;margin-top:4px}
.cluster{display:flex;align-items:center;gap:14px;flex-wrap:wrap;justify-content:flex-end}
.pill{display:inline-flex;align-items:center;gap:8px;padding:6px 12px;border-radius:999px;
  font-size:11px;letter-spacing:.18em;text-transform:uppercase;
  border:1px solid var(--line);background:rgba(0,240,255,.04);color:var(--fg)}
.pill .dot{width:7px;height:7px;border-radius:50%;background:var(--accent-3);box-shadow:0 0 10px var(--accent-3);animation:pulse 2s infinite}
.pill .dot.warn{background:var(--warn);box-shadow:0 0 10px var(--warn)}
.clock{font-family:'Orbitron',sans-serif;font-size:12px;letter-spacing:.22em;color:var(--accent);text-shadow:0 0 12px rgba(0,240,255,.6)}
.kpis{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-bottom:24px}
.kpi{position:relative;overflow:hidden;padding:16px 18px;border:1px solid var(--line);border-radius:14px;
  background:linear-gradient(180deg,rgba(12,19,34,.85),rgba(12,19,34,.55));backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
.kpi::before{content:"";position:absolute;inset:0;pointer-events:none;
  background:linear-gradient(120deg,transparent 30%,rgba(0,240,255,.08) 50%,transparent 70%);
  transform:translateX(-100%);transition:transform .8s}
.kpi:hover::before{transform:translateX(100%)}
.kpi h3{font-size:10px;letter-spacing:.32em;color:var(--fg-dim);text-transform:uppercase}
.kpi .v{font-family:'Orbitron',sans-serif;font-weight:700;font-size:clamp(22px,3.4vw,30px);color:var(--fg);margin-top:8px;line-height:1}
.kpi.cyan .v{color:var(--accent);text-shadow:0 0 16px rgba(0,240,255,.55)}
.kpi.magenta .v{color:var(--accent-2);text-shadow:0 0 16px rgba(255,43,214,.55)}
.kpi.lime .v{color:var(--accent-3);text-shadow:0 0 16px rgba(184,255,94,.45)}
.kpi .sub{font-size:10px;letter-spacing:.2em;color:var(--fg-dim);margin-top:6px;text-transform:uppercase}
.section-head{display:flex;align-items:baseline;justify-content:space-between;gap:16px;margin:8px 4px 12px}
.section-head h2{font-family:'Orbitron',sans-serif;font-weight:700;font-size:13px;letter-spacing:.32em;color:var(--fg);text-transform:uppercase}
.section-head .meta{font-size:11px;letter-spacing:.2em;color:var(--fg-dim);text-transform:uppercase}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:14px}
.card{position:relative;padding:16px;border:1px solid var(--line);border-radius:14px;
  background:linear-gradient(180deg,rgba(10,16,28,.9),rgba(10,16,28,.6));
  backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);
  transition:border-color .2s,transform .2s,box-shadow .2s}
.card:hover{border-color:var(--line-strong);transform:translateY(-1px);box-shadow:0 0 24px rgba(0,240,255,.08)}
.card .corner{position:absolute;width:12px;height:12px;border:1px solid var(--accent);opacity:.7}
.corner.tl{top:-1px;left:-1px;border-right:none;border-bottom:none}
.corner.tr{top:-1px;right:-1px;border-left:none;border-bottom:none}
.corner.bl{bottom:-1px;left:-1px;border-right:none;border-top:none}
.corner.br{bottom:-1px;right:-1px;border-left:none;border-top:none}
.card-head{display:flex;align-items:center;gap:10px;margin-bottom:12px}
.live{width:8px;height:8px;border-radius:50%;background:var(--accent-3);box-shadow:0 0 10px var(--accent-3);animation:pulse 1.6s infinite;flex:0 0 8px}
.id{font-family:'Orbitron',sans-serif;font-weight:700;font-size:14px;letter-spacing:.16em;color:var(--fg);flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.badge{font-size:10px;letter-spacing:.2em;text-transform:uppercase;padding:4px 10px;border-radius:999px;border:1px solid}
.badge.http{color:var(--accent);border-color:rgba(0,240,255,.35);background:rgba(0,240,255,.08)}
.badge.tcp{color:var(--accent-2);border-color:rgba(255,43,214,.35);background:rgba(255,43,214,.08)}
.badge.udp{color:var(--accent-3);border-color:rgba(184,255,94,.35);background:rgba(184,255,94,.08)}
.url-row{display:flex;align-items:center;gap:8px;padding:10px 12px;margin-bottom:12px;
  border:1px dashed var(--line-strong);border-radius:10px;background:rgba(0,240,255,.03)}
.url-row a{color:var(--accent);text-decoration:none;font-size:12px;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.url-row a:hover{text-decoration:underline}
.copy{background:transparent;border:1px solid var(--line);color:var(--fg-dim);
  padding:4px 8px;border-radius:6px;font-family:inherit;font-size:10px;letter-spacing:.16em;
  cursor:pointer;text-transform:uppercase;transition:all .15s}
.copy:hover{color:var(--accent);border-color:var(--accent)}
.copy.ok{color:var(--accent-3);border-color:var(--accent-3)}
.specs{display:grid;grid-template-columns:1fr 1fr;gap:8px 14px}
.spec{display:flex;flex-direction:column;gap:2px;min-width:0}
.spec dt{font-size:9px;letter-spacing:.24em;color:var(--fg-dim);text-transform:uppercase}
.spec dd{font-size:12px;color:var(--fg);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.spec dd.uptime{color:var(--accent);font-family:'Orbitron',sans-serif;letter-spacing:.08em}
.hero{position:relative;display:grid;grid-template-columns:minmax(0,360px) 1fr;gap:20px;align-items:stretch;margin-bottom:24px}
.hero .frame{position:relative;border:1px solid var(--line);border-radius:14px;overflow:hidden;
  background:radial-gradient(circle at 30% 30%,rgba(255,43,214,.12),transparent 60%),linear-gradient(180deg,rgba(8,13,24,.85),rgba(8,13,24,.55));
  backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);min-height:280px;display:grid;place-items:center;padding:12px}
.hero .frame::before,.hero .frame::after{content:"";position:absolute;left:0;right:0;height:24px;pointer-events:none;z-index:2}
.hero .frame::before{top:0;background:linear-gradient(180deg,rgba(4,6,11,.7),transparent)}
.hero .frame::after{bottom:0;background:linear-gradient(0deg,rgba(4,6,11,.7),transparent)}
.hero lottie-player,.hero dotlottie-player{width:100%;height:100%;max-height:380px;filter:drop-shadow(0 0 18px rgba(255,43,214,.25)) drop-shadow(0 0 24px rgba(0,240,255,.18))}
.hero .tag{position:absolute;top:10px;left:12px;z-index:3;font-family:'Orbitron',sans-serif;font-size:10px;letter-spacing:.32em;color:var(--accent);text-transform:uppercase;text-shadow:0 0 8px rgba(0,240,255,.6)}
.hero .sig{position:absolute;bottom:10px;right:12px;z-index:3;font-size:9px;letter-spacing:.24em;color:var(--fg-dim);text-transform:uppercase}
.hero .side{display:flex;flex-direction:column;justify-content:center;gap:14px;padding:18px;border:1px solid var(--line);border-radius:14px;
  background:linear-gradient(180deg,rgba(12,19,34,.85),rgba(12,19,34,.55));backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px)}
.hero .side h2{font-family:'Orbitron',sans-serif;font-weight:700;font-size:clamp(20px,2.6vw,28px);letter-spacing:.18em;text-transform:uppercase;color:var(--fg);line-height:1.15}
.hero .side h2 .accent{color:var(--accent-2);text-shadow:0 0 14px rgba(255,43,214,.55)}
.hero .side p{font-size:12px;color:var(--fg-dim);letter-spacing:.06em;line-height:1.7}
.hero .side .chips{display:flex;flex-wrap:wrap;gap:6px}
.hero .side .chip{font-size:9px;letter-spacing:.24em;text-transform:uppercase;padding:4px 10px;border:1px solid var(--line);border-radius:999px;color:var(--accent-3);background:rgba(184,255,94,.04)}
.empty{padding:60px 20px;text-align:center;color:var(--fg-dim);border:1px dashed var(--line);
  border-radius:14px;font-size:13px;letter-spacing:.16em;text-transform:uppercase}
.empty .big{display:block;font-family:'Orbitron',sans-serif;font-size:18px;color:var(--fg);letter-spacing:.32em;margin-bottom:14px}
.empty code{display:inline-block;margin-top:18px;padding:10px 16px;border:1px solid var(--line-strong);border-radius:8px;
  font-family:'JetBrains Mono',monospace;font-size:13px;color:var(--accent-3);background:rgba(184,255,94,.04);letter-spacing:.06em;text-transform:none}
.blink{color:var(--accent);animation:blink 1.2s steps(2,start) infinite}
@keyframes pulse{0%,100%{opacity:1;transform:scale(1)}50%{opacity:.55;transform:scale(.85)}}
@keyframes blink{50%{opacity:0}}
@media (max-width:900px){
  .hero{grid-template-columns:1fr}
  .hero lottie-player,.hero dotlottie-player{max-height:260px}
}
@media (max-width:720px){
  .kpis{grid-template-columns:repeat(2,1fr)}
  .brand h1{font-size:13px;letter-spacing:.22em}
  .topbar{flex-direction:column;align-items:stretch}
  .cluster{justify-content:space-between}
  .grid{grid-template-columns:1fr}
  .specs{grid-template-columns:1fr}
}
@media (prefers-reduced-motion:reduce){
  #bg{display:none}
  .pill .dot,.live,.blink,.kpi::before{animation:none}
}
`

// publicPageScripts is the shared animated background + live uptime ticker.
const publicPageScripts = `
(() => {
  const canvas = document.getElementById('bgcanvas');
  if (!canvas) return;
  const ctx = canvas.getContext('2d', { alpha: true });
  let w=0,h=0,dpr=Math.min(window.devicePixelRatio||1,2);
  let t0=performance.now();
  const nodes = Array.from({length:48},()=>({x:Math.random(),y:Math.random(),vx:(Math.random()-0.5)*0.00012,vy:(Math.random()-0.5)*0.00012}));
  function resize(){const r=canvas.parentElement.getBoundingClientRect();w=r.width;h=r.height;canvas.width=Math.floor(w*dpr);canvas.height=Math.floor(h*dpr);canvas.style.width=w+'px';canvas.style.height=h+'px';ctx.setTransform(dpr,0,0,dpr,0,0)}
  resize();window.addEventListener('resize',resize);
  function frame(now){const dt=now-t0;t0=now;ctx.clearRect(0,0,w,h);
    ctx.strokeStyle='rgba(0,240,255,0.06)';ctx.lineWidth=1;const step=60;const off=(now*0.012)%step;
    ctx.beginPath();
    for(let x=-step+off;x<w+step;x+=step){ctx.moveTo(x,0);ctx.lineTo(x+(h*0.15),h)}
    for(let y=-step+off;y<h+step;y+=step){ctx.moveTo(0,y);ctx.lineTo(w,y-(w*0.05))}
    ctx.stroke();
    for(const n of nodes){n.x+=n.vx*dt;n.y+=n.vy*dt;if(n.x<0||n.x>1)n.vx*=-1;if(n.y<0||n.y>1)n.vy*=-1}
    for(let i=0;i<nodes.length;i++){const a=nodes[i];
      for(let j=i+1;j<nodes.length;j++){const b=nodes[j];const dx=(a.x-b.x)*w,dy=(a.y-b.y)*h;const d2=dx*dx+dy*dy;
        if(d2<160*160){const al=(1-Math.sqrt(d2)/160)*0.35;ctx.strokeStyle='rgba(0,240,255,'+al.toFixed(3)+')';
          ctx.beginPath();ctx.moveTo(a.x*w,a.y*h);ctx.lineTo(b.x*w,b.y*h);ctx.stroke()}}}
    for(const n of nodes){ctx.fillStyle='rgba(184,255,94,0.55)';ctx.beginPath();ctx.arc(n.x*w,n.y*h,1.4,0,Math.PI*2);ctx.fill()}
    requestAnimationFrame(frame)}
  if(!window.matchMedia('(prefers-reduced-motion: reduce)').matches){requestAnimationFrame(frame)}
})();
const clock=document.getElementById('clock');
if(clock){setInterval(()=>{const d=new Date();const p=n=>String(n).padStart(2,'0');clock.textContent=p(d.getUTCHours())+':'+p(d.getUTCMinutes())+':'+p(d.getUTCSeconds())+' UTC'},1000)}
function fmtUp(u){if(!u)return '—';let s=Math.max(0,Math.floor(Date.now()/1000)-u);const d=Math.floor(s/86400);s%=86400;const h=Math.floor(s/3600);s%=3600;const m=Math.floor(s/60);s%=60;const p=n=>String(n).padStart(2,'0');return d>0?d+'d '+p(h)+':'+p(m)+':'+p(s):p(h)+':'+p(m)+':'+p(s)}
function tick(){document.querySelectorAll('[data-uptime]').forEach(el=>{el.textContent=fmtUp(parseInt(el.dataset.uptime,10))})}
tick();setInterval(tick,1000);
document.querySelectorAll('.copy').forEach(btn=>{btn.addEventListener('click',()=>{navigator.clipboard.writeText(btn.dataset.copy).then(()=>{btn.textContent='copied';btn.classList.add('ok');setTimeout(()=>{btn.textContent='copy';btn.classList.remove('ok')},1400)}).catch(()=>{btn.textContent='err'})})});
`

const publicPageHeadOpen = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>TUNNEL // RELAY</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Orbitron:wght@500;700;900&family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<script src="https://unpkg.com/@dotlottie/player-component@2.7.12/dist/dotlottie-player.mjs" type="module"></script>
<style>`

const publicPageHeadClose = `</style></head><body>
<div id="bg"><canvas id="bgcanvas"></canvas></div>
<div class="scanlines"></div>
<div class="vignette"></div>
<main>
<header class="topbar">
  <div class="brand">
    <div class="logo" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="#00f0ff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M2 12h6l3-9 3 18 3-9h5"/>
      </svg>
    </div>
    <div>
      <h1>Tunnel</h1>
      <small>Public Relay // Live</small>
    </div>
  </div>
  <div class="cluster">
    <span class="pill"><span class="dot"></span><span>Relay Online</span></span>
    <span class="clock" id="clock">--:--:-- UTC</span>
  </div>
</header>`

const publicPageFooter = `</main>
<script>` + publicPageScripts + `</script>
</body></html>`

// renderHero emits the cyberpunk hero panel when a Lottie URL is configured.
// When the URL is empty the function is a no-op so production deploys without
// the asset render cleanly.
func renderHero(w http.ResponseWriter, lottieURL string) {
	if lottieURL == "" {
		return
	}
	fmt.Fprintf(w, `<section class="hero" aria-label="cyberpunk hero">
<div class="frame">
  <span class="tag">// signal_01</span>
  <dotlottie-player src="%s" autoplay loop background="transparent" speed="1"></dotlottie-player>
  <span class="sig">render :: lottie</span>
</div>
<aside class="side">
  <h2>Relay <span class="accent">Online</span></h2>
  <p>Encrypted reverse tunnels. Public hostnames. Sub-second wake. The grid is live.</p>
  <div class="chips"><span class="chip">HTTPS</span><span class="chip">TCP</span><span class="chip">UDP</span><span class="chip">WS</span><span class="chip">SSE</span></div>
</aside>
</section>`, html.EscapeString(lottieURL))
}

func renderNoTunnelPage(w http.ResponseWriter) {
	renderNoTunnelPageWithHero(w, "")
}

func renderNoTunnelPageWithHero(w http.ResponseWriter, lottieURL string) {
	io.WriteString(w, publicPageHeadOpen)
	io.WriteString(w, publicPageStyles)
	io.WriteString(w, publicPageHeadClose)
	renderHero(w, lottieURL)
	io.WriteString(w, `<div class="empty"><span class="big">No tunnel connected</span>Awaiting handshake <span class="blink">_</span><br><br>Start one with<br><code>tunnel http &lt;port&gt;</code></div>`)
	io.WriteString(w, publicPageFooter)
}

func renderTunnelListPage(w http.ResponseWriter, tunnels []*Tunnel, cfg Config) {
	if len(tunnels) == 0 {
		renderNoTunnelPageWithHero(w, cfg.HeroLottieURL)
		return
	}

	byMode := map[string]int{"http": 0, "tcp": 0, "udp": 0}
	for _, t := range tunnels {
		byMode[t.Mode]++
	}

	io.WriteString(w, publicPageHeadOpen)
	io.WriteString(w, publicPageStyles)
	io.WriteString(w, publicPageHeadClose)
	renderHero(w, cfg.HeroLottieURL)

	fmt.Fprintf(w, `<section class="kpis" aria-label="summary">
<div class="kpi cyan"><h3>Active Tunnels</h3><div class="v">%d</div><div class="sub">live channels</div></div>
<div class="kpi magenta"><h3>HTTP</h3><div class="v">%d</div><div class="sub">L7 sessions</div></div>
<div class="kpi"><h3>TCP</h3><div class="v">%d</div><div class="sub">L4 streams</div></div>
<div class="kpi lime"><h3>UDP</h3><div class="v">%d</div><div class="sub">datagram</div></div>
</section>
<div class="section-head"><h2>Channels</h2><span class="meta">read-only public view</span></div>
<div class="grid">`,
		len(tunnels), byMode["http"], byMode["tcp"], byMode["udp"])

	now := time.Now()
	for _, t := range tunnels {
		mode := t.Mode
		if mode == "" {
			mode = "http"
		}
		var url, endpoint string
		switch {
		case t.PublicURL != "":
			url = t.PublicURL
		case cfg.BaseDomain != "":
			url = "https://" + t.ID + "." + cfg.BaseDomain
		default:
			url = "/t/" + t.ID + "/"
		}
		switch {
		case t.TCPAddr != "":
			endpoint = t.TCPAddr
		case t.UDPAddr != "":
			endpoint = t.UDPAddr
		default:
			endpoint = url
		}
		connected := t.ConnectedAt.UTC().Format("2006-01-02 15:04 UTC")
		uptime := now.Sub(t.ConnectedAt).Round(time.Second).String()

		fmt.Fprintf(w, `<article class="card">
<span class="corner tl"></span><span class="corner tr"></span><span class="corner bl"></span><span class="corner br"></span>
<div class="card-head"><span class="live" aria-hidden="true"></span><span class="id">%s</span><span class="badge %s">%s</span></div>
<div class="url-row"><a href="%s" target="_blank" rel="noopener">%s</a><button class="copy" data-copy="%s">copy</button></div>
<dl class="specs">
<div class="spec"><dt>Endpoint</dt><dd title="%s">%s</dd></div>
<div class="spec"><dt>Uptime</dt><dd class="uptime" data-uptime="%d">%s</dd></div>
<div class="spec"><dt>Connected</dt><dd>%s</dd></div>
<div class="spec"><dt>Protocol</dt><dd>%s</dd></div>
</dl>
</article>`,
			html.EscapeString(t.ID),
			html.EscapeString(mode), html.EscapeString(strings.ToUpper(mode)),
			html.EscapeString(url), html.EscapeString(url), html.EscapeString(url),
			html.EscapeString(endpoint), html.EscapeString(endpoint),
			t.ConnectedAt.Unix(), html.EscapeString(uptime),
			html.EscapeString(connected),
			html.EscapeString(strings.ToUpper(mode)),
		)
	}

	io.WriteString(w, `</div>`)
	io.WriteString(w, publicPageFooter)
}
