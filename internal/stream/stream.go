package stream

import (
	"net"
	"sync"
	"sync/atomic"
)

// Stream represents a multiplexed TCP connection over the WebSocket tunnel.
type Stream struct {
	ID     string
	Conn   net.Conn
	DataCh chan []byte
	done   chan struct{}
	closed atomic.Bool
}

// NewStream creates a new stream wrapping a TCP connection.
func NewStream(id string, conn net.Conn) *Stream {
	return &Stream{
		ID:     id,
		Conn:   conn,
		DataCh: make(chan []byte, 256),
		done:   make(chan struct{}),
	}
}

// Close marks the stream as closed and closes the underlying connection (if any).
// DataCh is intentionally left open — receivers select on Done() instead — so
// concurrent senders never panic on a closed channel.
func (s *Stream) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
		if s.Conn != nil {
			s.Conn.Close()
		}
	}
}

// Done returns a channel closed when Close() is called.
func (s *Stream) Done() <-chan struct{} {
	return s.done
}

// IsClosed returns whether the stream has been closed.
func (s *Stream) IsClosed() bool {
	return s.closed.Load()
}

// Registry manages a set of active streams.
type Registry struct {
	mu      sync.RWMutex
	streams map[string]*Stream
}

// NewRegistry creates a new stream registry.
func NewRegistry() *Registry {
	return &Registry{
		streams: make(map[string]*Stream),
	}
}

// Add registers a stream.
func (r *Registry) Add(s *Stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streams[s.ID] = s
}

// Get returns a stream by ID.
func (r *Registry) Get(id string) *Stream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.streams[id]
}

// Remove unregisters a stream.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, id)
}

// CloseAll closes and removes all streams.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.streams {
		s.Close()
		delete(r.streams, id)
	}
}
