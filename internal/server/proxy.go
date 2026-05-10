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

const noTunnelPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>No tunnel connected</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Inter',system-ui,sans-serif;background:#0F172A;color:#F8FAFC;min-height:100dvh;display:flex;align-items:center;justify-content:center}
.card{text-align:center;padding:48px 40px;max-width:480px;width:100%}
.dot{width:12px;height:12px;border-radius:50%;background:#EF4444;display:inline-block;margin-bottom:24px;animation:pulse 2s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1;transform:scale(1)}50%{opacity:.5;transform:scale(.85)}}
@media(prefers-reduced-motion:reduce){.dot{animation:none}}
h1{font-size:24px;font-weight:600;color:#F8FAFC;margin-bottom:12px;line-height:1.3}
p{font-size:15px;color:#94A3B8;line-height:1.6;margin-bottom:24px}
code{display:inline-block;background:#1E293B;border:1px solid #334155;border-radius:6px;padding:10px 16px;font-family:ui-monospace,'Cascadia Code',monospace;font-size:14px;color:#22C55E;letter-spacing:.01em}
</style>
</head>
<body>
<div class="card">
  <div class="dot"></div>
  <h1>No tunnel connected</h1>
  <p>Start a tunnel to expose your local server.</p>
  <code>tunnel http &lt;port&gt;</code>
</div>
</body>
</html>`

const tunnelListPageHeader = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Active tunnels</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:'Inter',system-ui,sans-serif;background:#0F172A;color:#F8FAFC;min-height:100dvh;display:flex;align-items:center;justify-content:center;padding:24px}
.card{background:#1E293B;border:1px solid #334155;border-radius:12px;padding:32px;width:100%;max-width:640px}
h1{font-size:18px;font-weight:600;color:#F8FAFC;margin-bottom:24px;display:flex;align-items:center;gap:8px}
.dot{width:8px;height:8px;border-radius:50%;background:#22C55E;flex-shrink:0}
table{width:100%;border-collapse:collapse;font-size:14px}
th{text-align:left;color:#64748B;font-weight:500;font-size:12px;text-transform:uppercase;letter-spacing:.05em;padding-bottom:12px;border-bottom:1px solid #334155}
td{padding:14px 0;border-bottom:1px solid #272F42;vertical-align:middle}
tr:last-child td{border-bottom:none}
.id{font-family:ui-monospace,'Cascadia Code',monospace;color:#CBD5E1}
.badge{display:inline-block;background:#272F42;border-radius:4px;padding:2px 8px;font-size:12px;color:#94A3B8;text-transform:uppercase;letter-spacing:.05em}
a{color:#22C55E;text-decoration:none;font-size:13px}
a:hover{text-decoration:underline}
a:focus{outline:2px solid #22C55E;outline-offset:2px;border-radius:2px}
</style>
</head>
<body>
<div class="card">
  <h1><span class="dot"></span>Active tunnels</h1>
  <table>
    <thead><tr><th>Name / ID</th><th>Mode</th><th>URL</th></tr></thead>
    <tbody>`

const tunnelListPageFooter = `    </tbody>
  </table>
</div>
</body>
</html>`

func renderNoTunnelPage(w http.ResponseWriter) {
	fmt.Fprint(w, noTunnelPageHTML)
}

func renderTunnelListPage(w http.ResponseWriter, tunnels []*Tunnel, cfg Config) {
	if len(tunnels) == 0 {
		fmt.Fprint(w, noTunnelPageHTML)
		return
	}
	fmt.Fprint(w, tunnelListPageHeader)
	for _, t := range tunnels {
		var url string
		if cfg.BaseDomain != "" {
			url = "https://" + html.EscapeString(t.ID) + "." + html.EscapeString(cfg.BaseDomain)
		} else {
			url = "/t/" + html.EscapeString(t.ID) + "/"
		}
		fmt.Fprintf(w, `<tr><td class="id">%s</td><td><span class="badge">%s</span></td><td><a href="%s">%s</a></td></tr>`,
			html.EscapeString(t.ID),
			html.EscapeString(t.Mode),
			url,
			html.EscapeString(url),
		)
	}
	fmt.Fprint(w, tunnelListPageFooter)
}
