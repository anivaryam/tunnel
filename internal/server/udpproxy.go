package server

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/anivaryam/tunnel/internal/safego"
	"github.com/anivaryam/tunnel/internal/stream"
	"github.com/google/uuid"
)

const (
	udpSessionTimeout         = 60 * time.Second
	udpSessionCleanupInterval = 10 * time.Second
)

type udpSession struct {
	streamID string
	addr     *net.UDPAddr
	lastSeen time.Time
}

// UDPProxy accepts incoming UDP packets and forwards them through a tunnel.
type UDPProxy struct {
	tunnel  *Tunnel
	metrics *Metrics
	conn    *net.UDPConn
	done    chan struct{}

	mu       sync.RWMutex
	sessions map[string]*udpSession // keyed by remote addr string
}

// NewUDPProxy creates a new UDP proxy bound to a specific tunnel.
func NewUDPProxy(tunnel *Tunnel, metrics *Metrics) *UDPProxy {
	return &UDPProxy{
		tunnel:   tunnel,
		metrics:  metrics,
		sessions: make(map[string]*udpSession),
		done:     make(chan struct{}),
	}
}

// Listen binds the UDP proxy to the given address without reading packets.
func (up *UDPProxy) Listen(addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	up.conn = conn
	log.Printf("[udp] listening on %s for tunnel %s", conn.LocalAddr(), up.tunnel.ID)
	return nil
}

// Addr returns the listener's local address, or nil if not listening.
func (up *UDPProxy) Addr() net.Addr {
	if up.conn != nil {
		return up.conn.LocalAddr()
	}
	return nil
}

// Serve reads packets from the connection. Blocks until the connection is closed
// or Stop() is called.
func (up *UDPProxy) Serve() error {
	safego.Go("udpproxy.cleanupLoop", up.cleanupLoop)

	buf := make([]byte, protocol.MaxStreamChunk)
	for {
		n, remoteAddr, err := up.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-up.done:
				return nil
			default:
				return err
			}
		}
		up.handlePacket(buf[:n], remoteAddr)
	}
}

// Stop stops the UDP proxy.
func (up *UDPProxy) Stop() {
	close(up.done)
}

// Close stops the UDP proxy.
func (up *UDPProxy) Close() error {
	if up.conn != nil {
		return up.conn.Close()
	}
	return nil
}

// SendToRemote sends a UDP packet back to the remote client for a given stream.
func (up *UDPProxy) SendToRemote(streamID string, data []byte) {
	up.mu.RLock()
	for _, sess := range up.sessions {
		if sess.streamID == streamID {
			if _, err := up.conn.WriteToUDP(data, sess.addr); err != nil {
				log.Printf("[udp] stream %s: write to client %s failed: %v", streamID, sess.addr, err)
			}
			break
		}
	}
	up.mu.RUnlock()
}

func (up *UDPProxy) handlePacket(data []byte, remoteAddr *net.UDPAddr) {
	addrKey := remoteAddr.String()
	tunnel := up.tunnel

	up.mu.Lock()
	sess, exists := up.sessions[addrKey]
	if exists {
		sess.lastSeen = time.Now()
		up.mu.Unlock()
	} else {
		// New session — create stream.
		streamID := uuid.New().String()
		sess = &udpSession{
			streamID: streamID,
			addr:     remoteAddr,
			lastSeen: time.Now(),
		}
		up.sessions[addrKey] = sess
		up.mu.Unlock()

		log.Printf("[udp] stream %s: new session from %s → tunnel %s", streamID, addrKey, tunnel.ID)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, err := tunnel.OpenStream(ctx, streamID, addrKey)
		if err != nil || !ok {
			log.Printf("[udp] stream %s: open failed: ok=%v err=%v", streamID, ok, err)
			up.mu.Lock()
			delete(up.sessions, addrKey)
			up.mu.Unlock()
			return
		}

		// Register a stream for receiving data back from client.
		// UDP doesn't have a net.Conn, so we use a dummy that the read loop writes to DataCh.
		s := stream.NewStream(streamID, nil)
		tunnel.Streams().Add(s)
		up.metrics.OnStreamOpen()

		// WS → UDP: read from DataCh, send back to remote.
		safego.Go("udpproxy.ws2udp", func() {
			for {
				select {
				case <-s.Done():
					return
				case pkt := <-s.DataCh:
					if _, err := up.conn.WriteToUDP(pkt, remoteAddr); err != nil {
						log.Printf("[udp] stream %s: write to remote error: %v", s.ID, err)
						return
					}
				}
			}
		})
	}

	// Forward the packet to client via WebSocket.
	env := protocol.Envelope{
		Type:       protocol.TypeStreamData,
		StreamData: &protocol.StreamDataPayload{StreamID: sess.streamID},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tunnel.Writer.WriteEnvelope(ctx, env, data); err != nil {
		log.Printf("[udp] stream %s: write error: %v", sess.streamID, err)
	}
}

func (up *UDPProxy) cleanupLoop() {
	ticker := time.NewTicker(udpSessionCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-up.done:
			return
		case <-ticker.C:
			now := time.Now()

			type expired struct {
				addr     string
				streamID string
			}
			var stale []expired

			up.mu.Lock()
			for addr, sess := range up.sessions {
				if now.Sub(sess.lastSeen) > udpSessionTimeout {
					stale = append(stale, expired{addr: addr, streamID: sess.streamID})
					delete(up.sessions, addr)
				}
			}
			up.mu.Unlock()

			for _, e := range stale {
				log.Printf("[udp] stream %s: session %s timed out", e.streamID, e.addr)
				tunnel := up.tunnel
				s := tunnel.Streams().Get(e.streamID)
				if s != nil {
					s.Close()
					tunnel.Streams().Remove(e.streamID)
					up.metrics.OnStreamClose()
				}
				closeEnv := protocol.Envelope{
					Type:        protocol.TypeStreamClose,
					StreamClose: &protocol.StreamClosePayload{StreamID: e.streamID},
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = tunnel.Writer.WriteEnvelope(ctx, closeEnv, nil)
				cancel()
			}
		}
	}
}
