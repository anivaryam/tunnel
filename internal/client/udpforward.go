package client

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
)

func (c *Client) handleUDPStreamOpen(ctx context.Context, env protocol.Envelope) {
	if env.StreamOpen == nil {
		return
	}
	streamID := env.StreamOpen.StreamID

	// Dial the local UDP port.
	addr := fmt.Sprintf("127.0.0.1:%d", c.LocalPort)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		log.Printf("[udp] stream %s: dial %s failed: %v", streamID, addr, err)
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

	ack := protocol.Envelope{
		Type: protocol.TypeStreamOpenAck,
		StreamOpenAck: &protocol.StreamOpenAckPayload{
			StreamID: streamID,
			OK:       true,
		},
	}
	if err := c.writer.WriteEnvelope(ctx, ack, nil); err != nil {
		log.Printf("[udp] stream %s: write ack failed: %v", streamID, err)
		conn.Close()
		return
	}

	s := stream.NewStream(streamID, conn)
	c.streams.Add(s)

	c.Display.PrintTCPStream(streamID, env.StreamOpen.RemoteAddr, true)

	// Local UDP → WebSocket: read from local conn, send stream_data to server.
	safego.Go("udpforward.local2ws", func() {
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
				return
			}
		}
	})

	// WebSocket → Local UDP: read from DataCh, write to local conn.
	safego.Go("udpforward.ws2local", func() {
		for {
			select {
			case <-s.Done():
				return
			case data := <-s.DataCh:
				if _, err := conn.Write(data); err != nil {
					log.Printf("[udp] stream %s: local write error: %v", streamID, err)
					s.Close()
					return
				}
			}
		}
	})
}
