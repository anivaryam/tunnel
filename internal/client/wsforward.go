package client

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/coder/websocket"
)

func (c *Client) handleWSOpen(ctx context.Context, env protocol.Envelope) {
	if env.WSOpen == nil {
		return
	}
	streamID := env.WSOpen.StreamID

	// Build WebSocket URL to local server.
	url := fmt.Sprintf("ws://127.0.0.1:%d%s", c.LocalPort, env.WSOpen.Path)
	if env.WSOpen.Query != "" {
		url += "?" + env.WSOpen.Query
	}

	// Forward headers, filtering out WebSocket-specific ones that the
	// library sets automatically during the handshake, and Origin which
	// dev servers (e.g. Vite 5.1+) check against their own host — the
	// browser sends the tunnel origin, which would cause a security rejection.
	httpHeaders := make(http.Header)
	for k, vals := range env.WSOpen.Headers {
		kLower := strings.ToLower(k)
		if strings.HasPrefix(kLower, "sec-websocket-") {
			continue
		}
		if kLower == "host" || kLower == "origin" {
			continue
		}
		for _, v := range vals {
			httpHeaders.Add(k, v)
		}
	}

	// Dial local WebSocket.
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: httpHeaders,
	})
	if err != nil {
		log.Printf("[ws] stream %s: dial local failed: %v", streamID, err)
		ack := protocol.Envelope{
			Type: protocol.TypeWSOpenAck,
			WSOpenAck: &protocol.WSOpenAckPayload{
				StreamID: streamID,
				OK:       false,
				Error:    err.Error(),
			},
		}
		c.writer.WriteEnvelope(ctx, ack, nil)
		return
	}
	conn.SetReadLimit(protocol.MaxBodySize)

	// Store the connection.
	c.wsMu.Lock()
	c.wsConns[streamID] = conn
	c.wsMu.Unlock()

	// Send success ack.
	ack := protocol.Envelope{
		Type: protocol.TypeWSOpenAck,
		WSOpenAck: &protocol.WSOpenAckPayload{
			StreamID: streamID,
			OK:       true,
		},
	}
	if err := c.writer.WriteEnvelope(ctx, ack, nil); err != nil {
		log.Printf("[ws] stream %s: write ack failed: %v", streamID, err)
		conn.CloseNow()
		c.wsMu.Lock()
		delete(c.wsConns, streamID)
		c.wsMu.Unlock()
		return
	}

	log.Printf("[ws] stream %s: connected to local WebSocket at %s", streamID, url)

	// Local WS → Tunnel: read from local WS, send ws_frame through tunnel.
	safego.Go("wsforward.local2tunnel", func() {
		defer func() {
			conn.CloseNow()
			c.wsMu.Lock()
			delete(c.wsConns, streamID)
			c.wsMu.Unlock()

			closeEnv := protocol.Envelope{
				Type:    protocol.TypeWSClose,
				WSClose: &protocol.WSClosePayload{StreamID: streamID},
			}
			c.writer.WriteEnvelope(ctx, closeEnv, nil)
		}()

		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			frameEnv := protocol.Envelope{
				Type: protocol.TypeWSFrame,
				WSFrame: &protocol.WSFramePayload{
					StreamID:    streamID,
					MessageType: int(msgType),
				},
			}
			if err := c.writer.WriteEnvelope(ctx, frameEnv, data); err != nil {
				return
			}
		}
	})
}

func (c *Client) handleWSFrame(env protocol.Envelope, body []byte) {
	if env.WSFrame == nil {
		return
	}
	c.wsMu.RLock()
	conn := c.wsConns[env.WSFrame.StreamID]
	c.wsMu.RUnlock()

	if conn == nil {
		return
	}

	msgType := websocket.MessageType(env.WSFrame.MessageType)
	if err := conn.Write(context.Background(), msgType, body); err != nil {
		log.Printf("[ws] stream %s: write to local failed: %v", env.WSFrame.StreamID, err)
	}
}

func (c *Client) handleWSClose(env protocol.Envelope) {
	if env.WSClose == nil {
		return
	}
	c.wsMu.Lock()
	conn := c.wsConns[env.WSClose.StreamID]
	delete(c.wsConns, env.WSClose.StreamID)
	c.wsMu.Unlock()

	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
}
