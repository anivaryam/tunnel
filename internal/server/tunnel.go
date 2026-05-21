package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
	"github.com/coder/websocket"
)

// WSStream represents a proxied WebSocket connection on the server side.
type WSStream struct {
	FrameCh chan WSFrame
	closed  atomic.Bool
}

// WSFrame is a single WebSocket message relayed through the tunnel.
type WSFrame struct {
	Type int // websocket.MessageType value (1=text, 2=binary)
	Data []byte
}

// Close closes the frame channel. Safe to call multiple times.
func (ws *WSStream) Close() {
	if ws.closed.CompareAndSwap(false, true) {
		close(ws.FrameCh)
	}
}

const (
	pingInterval     = 30 * time.Second
	pingWriteTimeout = 10 * time.Second
)

// cachedResponseTimeout is resolved once at startup. Tests that need a custom
// value can call resetResponseTimeoutForTest.
var cachedResponseTimeout = func() time.Duration {
	if v := os.Getenv("TUNNEL_RESPONSE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}()

// responseTimeout returns the configured response timeout, resolved once.
func responseTimeout() time.Duration { return cachedResponseTimeout }

// TunnelStats is a point-in-time snapshot of a tunnel's activity counters.
type TunnelStats struct {
	ActiveStreams   int
	PendingRequests int
	WSStreams       int
	SSEStreams      int
}

// Tunnel represents a single client connection and its pending request/response pairs.
type Tunnel struct {
	ID          string
	Mode        string // "http", "tcp", or "udp"
	RemoteAddr  string
	PublicURL   string // populated after registration; empty until then
	TCPAddr     string // public TCP listen address, mode == "tcp" only
	UDPAddr     string // public UDP listen address, mode == "udp" only
	ConnectedAt time.Time
	Conn        *websocket.Conn
	Writer      *protocol.ConnWriter

	mu         sync.Mutex
	cancel     context.CancelFunc
	pending    map[string]chan pendingResponse
	acks       map[string]chan protocol.StreamOpenAckPayload
	streams    *stream.Registry
	wsAcks     map[string]chan protocol.WSOpenAckPayload
	wsStreams  map[string]*WSStream
	sseStreams map[string]chan []byte // requestID → chunk channel
	closed     bool
}

type pendingResponse struct {
	Envelope protocol.Envelope
	Body     []byte
}

// NewTunnel creates a new Tunnel.
func NewTunnel(id, mode string, conn *websocket.Conn) *Tunnel {
	return &Tunnel{
		ID:          id,
		Mode:        mode,
		ConnectedAt: time.Now(),
		Conn:        conn,
		Writer:      protocol.NewConnWriter(conn),
		pending:     make(map[string]chan pendingResponse),
		acks:        make(map[string]chan protocol.StreamOpenAckPayload),
		streams:     stream.NewRegistry(),
		wsAcks:      make(map[string]chan protocol.WSOpenAckPayload),
		wsStreams:   make(map[string]*WSStream),
		sseStreams:  make(map[string]chan []byte),
	}
}

// Streams returns the tunnel's stream registry.
func (t *Tunnel) Streams() *stream.Registry {
	return t.streams
}

// Stats returns a point-in-time snapshot of the tunnel's activity counters.
func (t *Tunnel) Stats() TunnelStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TunnelStats{
		ActiveStreams:   t.streams.Len(),
		PendingRequests: len(t.pending),
		WSStreams:       len(t.wsStreams),
		SSEStreams:      len(t.sseStreams),
	}
}

// sendRequest sends an HTTP request envelope + body to the client and returns a channel
// that will receive the response.
func (t *Tunnel) sendRequest(ctx context.Context, env protocol.Envelope, body []byte) (chan pendingResponse, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("tunnel closed")
	}
	ch := make(chan pendingResponse, 1)
	t.pending[env.RequestID] = ch
	t.mu.Unlock()

	if err := t.Writer.WriteEnvelope(ctx, env, body); err != nil {
		t.mu.Lock()
		delete(t.pending, env.RequestID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write request: %w", err)
	}
	return ch, nil
}

// waitResponse waits for a response on the given channel with a timeout.
func (t *Tunnel) waitResponse(ch chan pendingResponse, requestID string) (protocol.Envelope, []byte, error) {
	timeout := responseTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp, ok := <-ch:
		if !ok {
			return protocol.Envelope{}, nil, errors.New("tunnel closed")
		}
		return resp.Envelope, resp.Body, nil
	case <-timer.C:
		t.mu.Lock()
		delete(t.pending, requestID)
		t.mu.Unlock()
		return protocol.Envelope{}, nil, fmt.Errorf("response timeout after %s", timeout)
	}
}

// OpenStream sends a stream_open to the client and waits for ack.
func (t *Tunnel) OpenStream(ctx context.Context, streamID, remoteAddr string) (bool, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false, errors.New("tunnel closed")
	}
	ackCh := make(chan protocol.StreamOpenAckPayload, 1)
	t.acks[streamID] = ackCh
	t.mu.Unlock()

	env := protocol.Envelope{
		Type: protocol.TypeStreamOpen,
		StreamOpen: &protocol.StreamOpenPayload{
			StreamID:   streamID,
			RemoteAddr: remoteAddr,
		},
	}
	if err := t.Writer.WriteEnvelope(ctx, env, nil); err != nil {
		t.mu.Lock()
		delete(t.acks, streamID)
		t.mu.Unlock()
		return false, fmt.Errorf("write stream_open: %w", err)
	}

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case ack := <-ackCh:
		return ack.OK, nil
	case <-timer.C:
		t.mu.Lock()
		delete(t.acks, streamID)
		t.mu.Unlock()
		return false, fmt.Errorf("stream open timeout")
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.acks, streamID)
		t.mu.Unlock()
		return false, ctx.Err()
	}
}

// OpenWSStream sends a ws_open to the client and waits for ack.
func (t *Tunnel) OpenWSStream(ctx context.Context, streamID, path, query string, headers map[string][]string) (bool, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false, errors.New("tunnel closed")
	}
	ackCh := make(chan protocol.WSOpenAckPayload, 1)
	t.wsAcks[streamID] = ackCh
	t.mu.Unlock()

	env := protocol.Envelope{
		Type: protocol.TypeWSOpen,
		WSOpen: &protocol.WSOpenPayload{
			StreamID: streamID,
			Path:     path,
			Query:    query,
			Headers:  headers,
		},
	}
	if err := t.Writer.WriteEnvelope(ctx, env, nil); err != nil {
		t.mu.Lock()
		delete(t.wsAcks, streamID)
		t.mu.Unlock()
		return false, fmt.Errorf("write ws_open: %w", err)
	}

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case ack := <-ackCh:
		if ack.OK {
			ws := &WSStream{FrameCh: make(chan WSFrame, 256)}
			t.mu.Lock()
			t.wsStreams[streamID] = ws
			t.mu.Unlock()
		}
		return ack.OK, nil
	case <-timer.C:
		t.mu.Lock()
		delete(t.wsAcks, streamID)
		t.mu.Unlock()
		return false, fmt.Errorf("ws open timeout")
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.wsAcks, streamID)
		t.mu.Unlock()
		return false, ctx.Err()
	}
}

// GetWSStream returns a WebSocket stream by ID.
func (t *Tunnel) GetWSStream(id string) *WSStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.wsStreams[id]
}

// RemoveWSStream removes and closes a WebSocket stream.
func (t *Tunnel) RemoveWSStream(id string) {
	t.mu.Lock()
	ws := t.wsStreams[id]
	delete(t.wsStreams, id)
	t.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
}

// SendWSClose notifies the client that a WebSocket stream has closed.
func (t *Tunnel) SendWSClose(ctx context.Context, streamID string) {
	env := protocol.Envelope{
		Type:    protocol.TypeWSClose,
		WSClose: &protocol.WSClosePayload{StreamID: streamID},
	}
	t.Writer.WriteEnvelope(ctx, env, nil)
}

// GetSSEStream returns the chunk channel registered for a streaming request.
// The channel is pre-registered when the TypeHTTPResponse{Streaming:true} arrives
// so chunks that arrive before proxyRequest starts draining are buffered.
func (t *Tunnel) GetSSEStream(requestID string) chan []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sseStreams[requestID]
}

// RemoveSSEStream removes the SSE stream reference. Any subsequent chunks for
// this request ID received in readLoop are silently discarded.
func (t *Tunnel) RemoveSSEStream(requestID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sseStreams, requestID)
}

// trySend attempts a non-blocking send on ch. Returns true if sent, false if ch is full.
// channelFull is a human-readable label for the log message.
func (t *Tunnel) trySend(ch chan []byte, body []byte, channelFull string) bool {
	select {
	case ch <- body:
		return true
	default:
		log.Printf("[tunnel %s] %s channel full, dropping", t.ID, channelFull)
		return false
	}
}

// trySendWS attempts a non-blocking send on wsCh. Returns true if sent, false if full.
func (t *Tunnel) trySendWS(wsCh chan WSFrame, frame WSFrame, label string) bool {
	select {
	case wsCh <- frame:
		return true
	default:
		log.Printf("[tunnel %s] ws stream %s frame channel full", t.ID, label)
		return false
	}
}

func (t *Tunnel) Serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t.cancel = cancel

	safego.Go("tunnel.pingLoop", func() { t.pingLoop(ctx) })
	t.readLoop(ctx)
}

// Stop cancels the tunnel's Serve context, signaling all goroutines to stop.
func (t *Tunnel) Stop() {
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *Tunnel) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingWriteTimeout)
			err := t.Conn.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("[tunnel %s] ping failed: %v", t.ID, err)
				t.Conn.CloseNow()
				return
			}
		}
	}
}

func (t *Tunnel) readLoop(ctx context.Context) {
	defer t.Close()

	for {
		env, body, err := protocol.ReadEnvelope(ctx, t.Conn)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				log.Printf("[tunnel %s] client disconnected normally", t.ID)
			} else {
				log.Printf("[tunnel %s] read error: %v", t.ID, err)
			}
			return
		}

		switch env.Type {
		case protocol.TypeHTTPResponse:
			t.mu.Lock()
			ch, ok := t.pending[env.RequestID]
			// Pre-register the SSE chunk channel while still holding the lock so
			// TypeHTTPStreamChunk messages that arrive before proxyRequest calls
			// GetSSEStream are buffered rather than dropped.
			if ok && env.HTTPResponse != nil && env.HTTPResponse.Streaming {
				t.sseStreams[env.RequestID] = make(chan []byte, 64)
			}
			if ok {
				delete(t.pending, env.RequestID)
			}
			t.mu.Unlock()
			if ok {
				ch <- pendingResponse{Envelope: env, Body: body}
			}

		case protocol.TypeHTTPStreamChunk:
			if env.HTTPStreamChunk != nil {
				t.mu.Lock()
				ch := t.sseStreams[env.HTTPStreamChunk.RequestID]
				t.mu.Unlock()
				if ch != nil {
					t.trySend(ch, body, "SSE stream "+env.HTTPStreamChunk.RequestID)
				}
			}

		case protocol.TypeHTTPStreamEnd:
			if env.HTTPStreamEnd != nil {
				t.mu.Lock()
				ch := t.sseStreams[env.HTTPStreamEnd.RequestID]
				delete(t.sseStreams, env.HTTPStreamEnd.RequestID)
				t.mu.Unlock()
				if ch != nil {
					close(ch)
				}
			}

		case protocol.TypeStreamOpenAck:
			if env.StreamOpenAck != nil {
				t.mu.Lock()
				ackCh, ok := t.acks[env.StreamOpenAck.StreamID]
				if ok {
					delete(t.acks, env.StreamOpenAck.StreamID)
				}
				t.mu.Unlock()
				if ok {
					ackCh <- *env.StreamOpenAck
				}
			}

		case protocol.TypeStreamData:
			if env.StreamData != nil {
				s := t.streams.Get(env.StreamData.StreamID)
				if s != nil && !s.IsClosed() {
					t.trySend(s.DataCh, body, "stream "+env.StreamData.StreamID+" data")
				}
			}

		case protocol.TypeStreamClose:
			if env.StreamClose != nil {
				s := t.streams.Get(env.StreamClose.StreamID)
				if s != nil {
					s.Close()
					t.streams.Remove(env.StreamClose.StreamID)
				}
			}

		case protocol.TypeWSOpenAck:
			if env.WSOpenAck != nil {
				t.mu.Lock()
				ackCh, ok := t.wsAcks[env.WSOpenAck.StreamID]
				if ok {
					delete(t.wsAcks, env.WSOpenAck.StreamID)
				}
				t.mu.Unlock()
				if ok {
					ackCh <- *env.WSOpenAck
				}
			}

		case protocol.TypeWSFrame:
			if env.WSFrame != nil {
				t.mu.Lock()
				ws := t.wsStreams[env.WSFrame.StreamID]
				t.mu.Unlock()
				if ws != nil && !ws.closed.Load() {
					t.trySendWS(ws.FrameCh, WSFrame{Type: env.WSFrame.MessageType, Data: body}, env.WSFrame.StreamID)
				}
			}

		case protocol.TypeWSClose:
			if env.WSClose != nil {
				t.RemoveWSStream(env.WSClose.StreamID)
			}

		default:
			log.Printf("[tunnel %s] unknown message type: %s", t.ID, env.Type)
		}
	}
}

// Close marks the tunnel as closed and drains all pending channels.
func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
	for id, ch := range t.acks {
		close(ch)
		delete(t.acks, id)
	}
	for id, ch := range t.wsAcks {
		close(ch)
		delete(t.wsAcks, id)
	}
	for id, ws := range t.wsStreams {
		ws.Close()
		delete(t.wsStreams, id)
	}
	for id, ch := range t.sseStreams {
		close(ch)
		delete(t.sseStreams, id)
	}
	t.streams.CloseAll()
}
