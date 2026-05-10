package client

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
)

func (c *Client) handleStreamOpen(ctx context.Context, env protocol.Envelope) {
	if env.StreamOpen == nil {
		return
	}
	streamID := env.StreamOpen.StreamID

	// Dial the local TCP port.
	addr := fmt.Sprintf("127.0.0.1:%d", c.LocalPort)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("[tcp] stream %s: dial %s failed: %v", streamID, addr, err)
		ack := protocol.Envelope{
			Type: protocol.TypeStreamOpenAck,
			StreamOpenAck: &protocol.StreamOpenAckPayload{
				StreamID: streamID,
				OK:       false,
				Error:    err.Error(),
			},
		}
		c.writer.WriteEnvelope(ctx, ack, nil)
		return
	}

	// Send success ack.
	ack := protocol.Envelope{
		Type: protocol.TypeStreamOpenAck,
		StreamOpenAck: &protocol.StreamOpenAckPayload{
			StreamID: streamID,
			OK:       true,
		},
	}
	if err := c.writer.WriteEnvelope(ctx, ack, nil); err != nil {
		log.Printf("[tcp] stream %s: write ack failed: %v", streamID, err)
		conn.Close()
		return
	}

	s := stream.NewStream(streamID, conn)
	c.streams.Add(s)

	c.Display.PrintTCPStream(streamID, env.StreamOpen.RemoteAddr, true)

	// Local TCP → WebSocket: read from local conn, send stream_data to server.
	safego.Go("tcpforward.local2ws", func() {
		defer func() {
			s.Close()
			c.streams.Remove(streamID)
			closeEnv := protocol.Envelope{
				Type:        protocol.TypeStreamClose,
				StreamClose: &protocol.StreamClosePayload{StreamID: streamID},
			}
			c.writer.WriteEnvelope(ctx, closeEnv, nil)
			c.Display.PrintTCPStream(streamID, env.StreamOpen.RemoteAddr, false)
		}()

		buf := make([]byte, protocol.MaxStreamChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				dataEnv := protocol.Envelope{
					Type:       protocol.TypeStreamData,
					StreamData: &protocol.StreamDataPayload{StreamID: streamID},
				}
				if wErr := c.writer.WriteEnvelope(ctx, dataEnv, buf[:n]); wErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[tcp] stream %s: local read error: %v", streamID, err)
				}
				return
			}
		}
	})

	// WebSocket → Local TCP: read from DataCh, write to local conn.
	safego.Go("tcpforward.ws2local", func() {
		for {
			select {
			case <-s.Done():
				return
			case data := <-s.DataCh:
				if _, err := conn.Write(data); err != nil {
					log.Printf("[tcp] stream %s: local write error: %v", streamID, err)
					s.Close()
					return
				}
			}
		}
	})
}

// trySendStream attempts a non-blocking send on ch. Returns true if sent, false if ch is full.
func trySendStream(ch chan []byte, body []byte, label string) bool {
	select {
	case ch <- body:
		return true
	default:
		log.Printf("[tcp] stream %s: data channel full", label)
		return false
	}
}

func (c *Client) handleStreamData(env protocol.Envelope, body []byte) {
	if env.StreamData == nil {
		return
	}
	s := c.streams.Get(env.StreamData.StreamID)
	if s != nil && !s.IsClosed() {
		trySendStream(s.DataCh, body, env.StreamData.StreamID)
	}
}

func (c *Client) handleStreamClose(env protocol.Envelope) {
	if env.StreamClose == nil {
		return
	}
	s := c.streams.Get(env.StreamClose.StreamID)
	if s != nil {
		s.Close()
		c.streams.Remove(env.StreamClose.StreamID)
	}
}
