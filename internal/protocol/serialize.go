package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/coder/websocket"
)

// ConnWriter wraps a WebSocket connection with a mutex to ensure atomic
// multi-frame writes (text envelope + binary body).
type ConnWriter struct {
	Conn *websocket.Conn
	mu   sync.Mutex
}

// NewConnWriter creates a ConnWriter for the given WebSocket connection.
func NewConnWriter(conn *websocket.Conn) *ConnWriter {
	return &ConnWriter{Conn: conn}
}

// WriteEnvelope sends an envelope as a text frame followed by body as a binary frame.
// The write is atomic -- no other write can interleave between the two frames.
func (cw *ConnWriter) WriteEnvelope(ctx context.Context, env Envelope, body []byte) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if body == nil {
		body = []byte{}
	}

	cw.mu.Lock()
	defer cw.mu.Unlock()

	if err := cw.Conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write text frame: %w", err)
	}
	if err := cw.Conn.Write(ctx, websocket.MessageBinary, body); err != nil {
		return fmt.Errorf("write binary frame: %w", err)
	}
	return nil
}

// WriteEnvelopeOnly writes just the text frame envelope without a binary frame.
func (cw *ConnWriter) WriteEnvelopeOnly(ctx context.Context, env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	cw.mu.Lock()
	defer cw.mu.Unlock()

	if err := cw.Conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write text frame: %w", err)
	}
	return nil
}

// ReadEnvelope reads an envelope (text frame) and body (binary frame) from the connection.
func ReadEnvelope(ctx context.Context, conn *websocket.Conn) (Envelope, []byte, error) {
	var env Envelope

	typ, data, err := conn.Read(ctx)
	if err != nil {
		return env, nil, fmt.Errorf("read text frame: %w", err)
	}
	if typ != websocket.MessageText {
		return env, nil, fmt.Errorf("expected text frame, got %v", typ)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, nil, fmt.Errorf("unmarshal envelope: %w", err)
	}

	typ, body, err := conn.Read(ctx)
	if err != nil {
		return env, nil, fmt.Errorf("read binary frame: %w", err)
	}
	if typ != websocket.MessageBinary {
		return env, nil, fmt.Errorf("expected binary frame, got %v", typ)
	}
	if int64(len(body)) > MaxBodySize {
		return env, nil, fmt.Errorf("body too large: %d bytes (max %d)", len(body), MaxBodySize)
	}

	return env, body, nil
}

// ReadEnvelopeOnly reads just the text frame envelope without expecting a binary frame.
// Used for reading the initial assignment message.
func ReadEnvelopeOnly(ctx context.Context, conn *websocket.Conn) (Envelope, error) {
	var env Envelope

	typ, data, err := conn.Read(ctx)
	if err != nil {
		return env, fmt.Errorf("read text frame: %w", err)
	}
	if typ != websocket.MessageText {
		return env, fmt.Errorf("expected text frame, got %v", typ)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("unmarshal envelope: %w", err)
	}

	return env, nil
}

// ReadBody reads the request body up to MaxBodySize.
func ReadBody(r io.Reader) ([]byte, error) {
	lr := io.LimitReader(r, MaxBodySize+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxBodySize {
		return nil, fmt.Errorf("body too large (max %d bytes)", MaxBodySize)
	}
	return data, nil
}
