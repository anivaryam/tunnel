package client

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
	"github.com/coder/websocket"
)

const (
	pingInterval     = 30 * time.Second
	pingWriteTimeout = 10 * time.Second
)

// EventSink, if non-nil, is invoked with structured events that mirror what
// Display prints. Used by the daemon mode to broadcast over IPC. The default
// build leaves this nil and pays no cost.
type EventSink interface {
	OnLog(line string, isError bool)
	OnTunnelURL(url string)
	OnState(state string) // connecting | connected | reconnecting | exited
}

// maxInflightRequests bounds the number of concurrent in-flight HTTP requests
// per tunnel client. Beyond this, additional incoming TypeHTTPRequest envelopes
// are answered with 503 instead of spawning unbounded goroutines that each open
// a localhost connection.
const maxInflightRequests = 256

// Client manages the WebSocket connection to the relay server.
type Client struct {
	ServerURL string
	Token     string
	LocalPort int
	Mode      string // "http" (default) or "tcp"
	Name      string // optional: requested stable tunnel name
	Inspect   bool
	Display   *Display
	Sink      EventSink // optional; nil in default build
	inspector *Inspector

	writer   *protocol.ConnWriter
	conn     *websocket.Conn
	streams  *stream.Registry
	tunnelMu sync.RWMutex
	tunnelID string

	wsMu    sync.RWMutex
	wsConns map[string]*websocket.Conn

	// Request cancel functions keyed by request ID so the server's
	// TypeHTTPStreamCancel message can abort local HTTP work.
	requestMu      sync.Mutex
	requestCancels map[string]context.CancelFunc

	// Upload streaming: pipe writers keyed by request ID.
	// TypeHTTPRequestChunk messages are written to the pipe; TypeHTTPRequestEnd closes it.
	reqPipesMu sync.Mutex
	reqPipes   map[string]*io.PipeWriter

	// inflight bounds concurrent handleRequest goroutines.
	inflight chan struct{}
}

// resetConnState releases all per-connection stream bookkeeping. It is called
// when a connection ends (disconnect or shutdown) so the maps don't accumulate
// stale, unreachable entries across reconnects. Each map is drained under its
// own lock and the entries are actively closed/cancelled (not just dropped) so
// any local goroutines blocked on them unwind.
func (c *Client) resetConnState() {
	c.wsMu.Lock()
	for id, conn := range c.wsConns {
		if conn != nil {
			conn.Close(websocket.StatusGoingAway, "tunnel disconnected")
		}
		delete(c.wsConns, id)
	}
	c.wsMu.Unlock()

	c.requestMu.Lock()
	for id, cancel := range c.requestCancels {
		if cancel != nil {
			cancel()
		}
		delete(c.requestCancels, id)
	}
	c.requestMu.Unlock()

	c.reqPipesMu.Lock()
	for id, pw := range c.reqPipes {
		if pw != nil {
			pw.CloseWithError(fmt.Errorf("tunnel disconnected"))
		}
		delete(c.reqPipes, id)
	}
	c.reqPipesMu.Unlock()
}

// TunnelID returns the tunnel ID assigned by the server, or "" if not yet connected.
func (c *Client) TunnelID() string {
	c.tunnelMu.RLock()
	defer c.tunnelMu.RUnlock()
	return c.tunnelID
}

// NewClient creates a new tunnel client.
func NewClient(serverURL, token string, localPort int) *Client {
	return &Client{
		ServerURL:      serverURL,
		Token:          token,
		LocalPort:      localPort,
		Mode:           "http",
		Display:        NewDisplay(),
		streams:        stream.NewRegistry(),
		wsConns:        make(map[string]*websocket.Conn),
		requestCancels: make(map[string]context.CancelFunc),
		reqPipes:       make(map[string]*io.PipeWriter),
		inflight:       make(chan struct{}, maxInflightRequests),
	}
}

// friendlyDialError unwraps the deeply-nested websocket-dial / net error
// chain and returns a one-line diagnosis the user can act on. Without this
// the typical failure message is four-levels-deep "dial: failed to
// WebSocket dial: failed to send handshake request: Get \"...\": dial tcp
// [::1]:8080: connect: connection refused" which obscures the simple cause.
func friendlyDialError(serverURL string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("relay %s is not reachable (connection refused); is the relay running and is the URL correct? try `tunnel doctor`", serverURL)
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "name resolution"):
		return fmt.Errorf("relay %s: DNS lookup failed; is the hostname correct? try `tunnel doctor`", serverURL)
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return fmt.Errorf("relay %s: connection timed out. Network or firewall issue?", serverURL)
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"), strings.Contains(msg, "certificate"):
		return fmt.Errorf("relay %s: TLS handshake failed (%v)", serverURL, err)
	case strings.Contains(msg, "401"):
		return fmt.Errorf("relay %s: unauthorized - token missing or invalid; set with `tunnel config set-token <token>`", serverURL)
	case strings.Contains(msg, "429"):
		return fmt.Errorf("relay %s: rejected with 429 too many requests - per-token or per-IP cap reached", serverURL)
	}
	return fmt.Errorf("dial: %w", err)
}

// isFatalConnectError reports whether err represents a permanent
// configuration problem that won't be cured by retrying. These short-circuit
// the reconnect loop so the user sees a clear error instead of an
// exponential-backoff retry storm.
func isFatalConnectError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to parse url"),
		strings.Contains(msg, "invalid character"),
		strings.Contains(msg, "no Host in request URL"),
		strings.Contains(msg, "missing protocol scheme"),
		strings.Contains(msg, "unsupported protocol scheme"):
		return true
	}
	return false
}

// Run connects to the relay and processes requests. Reconnects on failure.
func (c *Client) Run(ctx context.Context) error {
	if c.Inspect {
		c.inspector = NewInspector()
		if ok, errMsg := c.inspector.Start("127.0.0.1:4040"); !ok {
			log.Printf("inspector disabled: %s", errMsg)
			c.Inspect = false
			c.inspector = nil
		}
	}

	if c.Sink != nil {
		c.Display.attachSink(c.Sink)
		c.Sink.OnState("connecting")
		defer c.Sink.OnState("exited")
	}

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		connectedOK, err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Fast-fail on permanent configuration errors (bad URL, parse error).
		// Reconnect loops here only confuse the user — the relay URL is wrong
		// and waiting won't fix it.
		if err != nil && isFatalConnectError(err) {
			c.Display.PrintDisconnected(err)
			return fmt.Errorf("relay URL is invalid; fix with `tunnel config set-server <url>`: %w", err)
		}

		// Reset backoff on successful connection (ran for a while before disconnect).
		if connectedOK {
			backoff = time.Second
		}

		log.Printf("disconnected: %v", err)
		c.Display.PrintDisconnected(err)

		// Add jitter (±25%) to prevent thundering herd on relay restart.
		// Use crypto/rand for jitter to avoid gosec G404 warning.
		var jitterN int64
		if n, rErr := cryptoRand.Int(cryptoRand.Reader, big.NewInt(int64(backoff/2))); rErr == nil {
			jitterN = n.Int64()
		}
		jitter := time.Duration(jitterN) - backoff/4
		wait := backoff + jitter

		if c.Sink != nil {
			c.Sink.OnState("reconnecting")
		}

		log.Printf("reconnecting in %s...", wait)
		c.Display.PrintReconnecting(wait)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) (connectedOK bool, err error) {
	c.tunnelMu.RLock()
	currentID := c.tunnelID
	c.tunnelMu.RUnlock()

	wsURL := fmt.Sprintf("%s/ws/connect?mode=%s", c.ServerURL, c.Mode)
	if c.Name != "" {
		wsURL += "&name=" + url.QueryEscape(c.Name)
	} else if currentID != "" {
		wsURL += "&tunnel_id=" + currentID
	}

	// Send auth token in Authorization header instead of query string to avoid log exposure.
	dialOpts := &websocket.DialOptions{}
	if c.Token != "" {
		dialOpts.HTTPHeader = http.Header{
			"Authorization": []string{"Bearer " + c.Token},
		}
	}

	conn, _, dialErr := websocket.Dial(ctx, wsURL, dialOpts)
	if dialErr != nil {
		return false, friendlyDialError(c.ServerURL, dialErr)
	}
	defer conn.CloseNow()

	conn.SetReadLimit(protocol.MaxBodySize + 4096)
	c.conn = conn
	c.writer = protocol.NewConnWriter(conn)

	// Tear down any per-connection stream state when this connection ends so a
	// reconnect starts clean instead of leaking ws conns, request cancels, and
	// upload pipes keyed by request IDs that will never arrive again.
	defer c.resetConnState()

	// Read the tunnel assignment.
	assignment, assignErr := protocol.ReadEnvelopeOnly(ctx, conn)
	if assignErr != nil {
		return false, fmt.Errorf("read assignment: %w", assignErr)
	}
	if assignment.Assignment == nil {
		return false, fmt.Errorf("expected assignment message")
	}

	c.tunnelMu.Lock()
	reconnected := c.tunnelID != "" && assignment.Assignment.TunnelID == c.tunnelID
	c.tunnelID = assignment.Assignment.TunnelID
	c.tunnelMu.Unlock()

	if c.Sink != nil {
		c.Sink.OnState("connected")
	}

	switch {
	case reconnected:
		c.Display.PrintReconnected(assignment.Assignment.PublicURL)
	case c.Mode == "tcp":
		c.Display.PrintTCPBanner(assignment.Assignment.TCPAddr, c.LocalPort)
	case c.Mode == "udp":
		c.Display.PrintUDPBanner(assignment.Assignment.UDPAddr, c.LocalPort)
	default:
		c.Display.PrintBanner(assignment.Assignment.PublicURL, c.LocalPort, c.Inspect)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	safego.Go("client.pingLoop", func() { c.pingLoop(ctx) })

	// Process messages until disconnected.
	for {
		env, body, err := protocol.ReadEnvelope(ctx, conn)
		if err != nil {
			return true, fmt.Errorf("read message: %w", err)
		}

		switch env.Type {
		case protocol.TypeHTTPRequest:
			if env.HTTPRequest != nil && env.HTTPRequest.Streaming {
				// Large upload: body arrives via TypeHTTPRequestChunk messages.
				pr, pw := io.Pipe()
				c.reqPipesMu.Lock()
				c.reqPipes[env.RequestID] = pw
				c.reqPipesMu.Unlock()
				c.dispatchRequest(ctx, env, pr)
			} else {
				var bodyReader io.Reader
				if len(body) > 0 {
					bodyReader = bytes.NewReader(body)
				}
				c.dispatchRequest(ctx, env, bodyReader)
			}
		case protocol.TypeHTTPRequestChunk:
			if env.HTTPRequestChunk != nil {
				c.reqPipesMu.Lock()
				pw := c.reqPipes[env.HTTPRequestChunk.RequestID]
				c.reqPipesMu.Unlock()
				if pw != nil {
					if _, err := pw.Write(body); err != nil {
						// Local request was cancelled; close the pipe.
						c.reqPipesMu.Lock()
						delete(c.reqPipes, env.HTTPRequestChunk.RequestID)
						c.reqPipesMu.Unlock()
						pw.CloseWithError(err)
					}
				}
			}
		case protocol.TypeHTTPRequestEnd:
			if env.HTTPRequestEnd != nil {
				c.reqPipesMu.Lock()
				pw := c.reqPipes[env.HTTPRequestEnd.RequestID]
				delete(c.reqPipes, env.HTTPRequestEnd.RequestID)
				c.reqPipesMu.Unlock()
				if pw != nil {
					pw.Close()
				}
			}
		case protocol.TypeStreamOpen:
			if c.Mode == "udp" {
				safego.Go("client.handleUDPStreamOpen", func() { c.handleUDPStreamOpen(ctx, env) })
			} else {
				safego.Go("client.handleStreamOpen", func() { c.handleStreamOpen(ctx, env) })
			}
		case protocol.TypeStreamData:
			c.handleStreamData(env, body)
		case protocol.TypeStreamClose:
			c.handleStreamClose(env)
		case protocol.TypeWSOpen:
			safego.Go("client.handleWSOpen", func() { c.handleWSOpen(ctx, env) })
		case protocol.TypeWSFrame:
			c.handleWSFrame(env, body)
		case protocol.TypeWSClose:
			c.handleWSClose(env)
		case protocol.TypeHTTPStreamCancel:
			if env.HTTPStreamCancel != nil {
				c.cancelRequest(env.HTTPStreamCancel.RequestID)
			}
		default:
			log.Printf("unknown message type: %s", env.Type)
		}
	}
}

func (c *Client) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingWriteTimeout)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("ping failed: %v", err)
				c.conn.CloseNow()
				return
			}
		}
	}
}

// dispatchRequest spawns a bounded handleRequest goroutine. If the inflight
// semaphore is full, the request is answered with 503 Service Unavailable
// instead of unbounded goroutine spawn — surfaces backpressure to the relay
// rather than collapsing the local app.
func (c *Client) dispatchRequest(ctx context.Context, env protocol.Envelope, bodyReader io.Reader) {
	select {
	case c.inflight <- struct{}{}:
		safego.Go("client.handleRequest", func() {
			defer func() { <-c.inflight }()
			c.handleRequest(ctx, env, bodyReader)
		})
	default:
		log.Printf("inflight cap reached (%d); responding 503 for %s", maxInflightRequests, env.RequestID)
		errEnv := protocol.Envelope{
			Type:      protocol.TypeHTTPResponse,
			RequestID: env.RequestID,
			HTTPResponse: &protocol.HTTPResponsePayload{
				StatusCode:    503,
				Headers:       map[string][]string{"Content-Type": {"text/plain"}, "Retry-After": {"1"}},
				ContentLength: int64(len("Service Unavailable")),
			},
		}
		_ = c.writer.WriteEnvelope(ctx, errEnv, []byte("Service Unavailable"))
		// drain pipe reader if streaming upload, so the relay's stream is freed.
		if pr, ok := bodyReader.(*io.PipeReader); ok {
			_ = pr.CloseWithError(fmt.Errorf("inflight cap"))
		}
	}
}

func (c *Client) handleRequest(ctx context.Context, env protocol.Envelope, bodyReader io.Reader) {
	if env.HTTPRequest == nil {
		log.Printf("received non-request envelope type: %s", env.Type)
		return
	}

	start := time.Now()
	req := env.HTTPRequest

	// Build the local URL.
	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", c.LocalPort, req.Path)
	if req.Query != "" {
		localURL += "?" + req.Query
	}

	// Create a per-request cancellable context so TypeHTTPStreamCancel (sent by
	// the server when the browser closes a stream or a relay timeout fires) can
	// abort the local HTTP request and free the goroutine.
	reqCtx, reqCancel := context.WithCancel(ctx)
	c.requestMu.Lock()
	c.requestCancels[env.RequestID] = reqCancel
	c.requestMu.Unlock()
	defer func() {
		c.requestMu.Lock()
		delete(c.requestCancels, env.RequestID)
		c.requestMu.Unlock()
		reqCancel()
	}()

	httpReq, err := http.NewRequestWithContext(reqCtx, req.Method, localURL, bodyReader)
	if err != nil {
		log.Printf("build request: %v", err)
		return
	}
	for k, vals := range req.Headers {
		if !protocol.HopByHopHeaders[http.CanonicalHeaderKey(k)] {
			for _, v := range vals {
				httpReq.Header.Add(k, v)
			}
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("forward error: %v", err)
		errEnv := protocol.Envelope{
			Type:      protocol.TypeHTTPResponse,
			RequestID: env.RequestID,
			HTTPResponse: &protocol.HTTPResponsePayload{
				StatusCode:    502,
				Headers:       map[string][]string{"Content-Type": {"text/plain"}},
				ContentLength: int64(len("Bad Gateway")),
			},
		}
		c.writer.WriteEnvelope(ctx, errEnv, []byte("Bad Gateway"))
		return
	}
	defer resp.Body.Close()

	// Filter hop-by-hop headers from the response.
	respHeaders := make(map[string][]string, len(resp.Header))
	for k, vals := range resp.Header {
		if !protocol.HopByHopHeaders[http.CanonicalHeaderKey(k)] {
			respHeaders[k] = vals
		}
	}

	// Decide whether to stream or buffer this response.
	//
	// HTML must always be buffered so the server can inject the rewrite script.
	//
	// SSE (text/event-stream) must always be streamed — the body is infinite.
	//
	// Everything else: buffer if the size is known and small (≤ 1 MB) so that
	// JS/CSS modules arrive with a correct Content-Length and full content in
	// one shot (chunked-encoding without Content-Length confuses some ES module
	// loaders). Stream if the size is unknown or large.
	const downloadStreamThreshold = 1 * 1024 * 1024 // 1 MB
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.HasPrefix(ct, "text/html")
	isSSE := strings.HasPrefix(ct, "text/event-stream")
	shouldStream := isSSE || (!isHTML && (resp.ContentLength < 0 || resp.ContentLength > downloadStreamThreshold))

	if shouldStream {
		c.streamResponse(reqCtx, env, resp, respHeaders, start)
		return
	}

	// Buffered path: HTML (for rewrite script injection) and small known-size responses.
	respBody, err := protocol.ReadBody(resp.Body)
	if err != nil {
		log.Printf("read response body: %v", err)
		return
	}

	respEnv := protocol.Envelope{
		Type:      protocol.TypeHTTPResponse,
		RequestID: env.RequestID,
		HTTPResponse: &protocol.HTTPResponsePayload{
			StatusCode:    resp.StatusCode,
			Headers:       respHeaders,
			ContentLength: int64(len(respBody)),
		},
	}

	elapsed := time.Since(start)
	c.Display.PrintRequest(req.Method, req.Path, resp.StatusCode, elapsed)

	if c.inspector != nil {
		c.inspector.Add(InspectEntry{
			ID:              env.RequestID,
			Timestamp:       start,
			Method:          req.Method,
			Path:            req.Path,
			Query:           req.Query,
			RequestHeaders:  req.Headers,
			RequestBody:     "", // body is streamed; not available for inspection
			StatusCode:      resp.StatusCode,
			ResponseHeaders: respHeaders,
			ResponseBody:    string(respBody),
			Duration:        elapsed,
			DurationStr:     elapsed.Round(time.Millisecond).String(),
		})
	}

	if err := c.writer.WriteEnvelope(ctx, respEnv, respBody); err != nil {
		log.Printf("write response error: %v", err)
	}
}

// streamResponse sends a streaming (SSE) response through the tunnel. It sends
// the response headers immediately with Streaming:true, then relays body chunks
// until EOF, error, or the context is cancelled (via TypeHTTPStreamCancel).
func (c *Client) streamResponse(ctx context.Context, env protocol.Envelope, resp *http.Response, respHeaders map[string][]string, start time.Time) {
	headersEnv := protocol.Envelope{
		Type:      protocol.TypeHTTPResponse,
		RequestID: env.RequestID,
		HTTPResponse: &protocol.HTTPResponsePayload{
			StatusCode: resp.StatusCode,
			Headers:    respHeaders,
			Streaming:  true,
		},
	}
	if err := c.writer.WriteEnvelope(ctx, headersEnv, nil); err != nil {
		return
	}
	c.Display.PrintRequest(env.HTTPRequest.Method, env.HTTPRequest.Path, resp.StatusCode, time.Since(start))

	buf := make([]byte, protocol.MaxStreamChunk)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunkEnv := protocol.Envelope{
				Type:            protocol.TypeHTTPStreamChunk,
				HTTPStreamChunk: &protocol.HTTPStreamChunkPayload{RequestID: env.RequestID},
			}
			if writeErr := c.writer.WriteEnvelope(ctx, chunkEnv, buf[:n]); writeErr != nil {
				return
			}
		}
		if readErr != nil {
			break
		}
	}

	// Signal end of stream regardless of whether it closed normally or on error.
	if err := c.writer.WriteEnvelope(ctx, protocol.Envelope{
		Type:          protocol.TypeHTTPStreamEnd,
		HTTPStreamEnd: &protocol.HTTPStreamEndPayload{RequestID: env.RequestID},
	}, nil); err != nil {
		log.Printf("write stream end: %v", err)
	}
}

// cancelRequest cancels local HTTP work for a request ID.
func (c *Client) cancelRequest(requestID string) {
	c.requestMu.Lock()
	cancel, ok := c.requestCancels[requestID]
	if ok {
		delete(c.requestCancels, requestID)
	}
	c.requestMu.Unlock()
	if ok {
		cancel()
	}
}
