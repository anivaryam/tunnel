package server

import (
	"context"
	"io"
	"log"
	"net"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
	"github.com/google/uuid"
)

// TCPProxy accepts incoming TCP connections and forwards them through a tunnel.
type TCPProxy struct {
	tunnel   *Tunnel
	metrics  *Metrics
	listener net.Listener
}

// NewTCPProxy creates a new TCP proxy bound to a specific tunnel.
func NewTCPProxy(tunnel *Tunnel, metrics *Metrics) *TCPProxy {
	return &TCPProxy{tunnel: tunnel, metrics: metrics}
}

// Listen binds the TCP proxy to the given address without accepting connections.
func (tp *TCPProxy) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tp.listener = ln
	log.Printf("[tcp] listening on %s for tunnel %s", ln.Addr(), tp.tunnel.ID)
	return nil
}

// Addr returns the listener's address, or nil if not listening.
func (tp *TCPProxy) Addr() net.Addr {
	if tp.listener != nil {
		return tp.listener.Addr()
	}
	return nil
}

// Serve accepts connections on the listener. Blocks until the listener is closed.
func (tp *TCPProxy) Serve() error {
	for {
		conn, err := tp.listener.Accept()
		if err != nil {
			return err
		}
		c := conn
		safego.Go("tcpproxy.handleConn", func() { tp.handleConn(c) })
	}
}

// Close stops the TCP proxy listener.
func (tp *TCPProxy) Close() error {
	if tp.listener != nil {
		return tp.listener.Close()
	}
	return nil
}

func (tp *TCPProxy) handleConn(conn net.Conn) {
	defer conn.Close()

	tunnel := tp.tunnel
	streamID := uuid.New().String()
	remoteAddr := conn.RemoteAddr().String()

	log.Printf("[tcp] stream %s: new connection from %s → tunnel %s", streamID, remoteAddr, tunnel.ID)

	// Ask the client to open a local TCP connection.
	ctx := context.Background()
	ok, err := tunnel.OpenStream(ctx, streamID, remoteAddr)
	if err != nil || !ok {
		log.Printf("[tcp] stream %s: open failed: ok=%v err=%v", streamID, ok, err)
		return
	}

	// Register the stream so the read loop can deliver data to it.
	s := stream.NewStream(streamID, conn)
	tunnel.Streams().Add(s)
	tp.metrics.OnStreamOpen()
	defer func() {
		s.Close()
		tunnel.Streams().Remove(streamID)
		tp.metrics.OnStreamClose()
		// Notify client the stream is closed.
		closeEnv := protocol.Envelope{
			Type:        protocol.TypeStreamClose,
			StreamClose: &protocol.StreamClosePayload{StreamID: streamID},
		}
		tunnel.Writer.WriteEnvelope(ctx, closeEnv, nil)
	}()

	done := make(chan struct{})

	// TCP → WebSocket: read from remote TCP, send stream_data to client.
	safego.Go("tcpproxy.tcp2ws", func() {
		defer close(done)
		buf := make([]byte, protocol.MaxStreamChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				env := protocol.Envelope{
					Type:       protocol.TypeStreamData,
					StreamData: &protocol.StreamDataPayload{StreamID: streamID},
				}
				if wErr := tunnel.Writer.WriteEnvelope(ctx, env, buf[:n]); wErr != nil {
					log.Printf("[tcp] stream %s: write to WS error: %v", streamID, wErr)
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("[tcp] stream %s: read from TCP error: %v", streamID, err)
				}
				return
			}
		}
	})

	// WebSocket → TCP: read from stream's DataCh, write to remote TCP.
	safego.Go("tcpproxy.ws2tcp", func() {
		for {
			select {
			case <-s.Done():
				return
			case data := <-s.DataCh:
				if _, err := conn.Write(data); err != nil {
					log.Printf("[tcp] stream %s: write to TCP error: %v", streamID, err)
					s.Close()
					return
				}
			}
		}
	})

	<-done
	log.Printf("[tcp] stream %s: closed", streamID)
}
