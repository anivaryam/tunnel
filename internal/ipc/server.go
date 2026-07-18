//go:build daemon

package ipc

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"
)

const maxHistory = 500

// Server is a Unix-socket / named-pipe IPC server. Single-tunnel: one state.
type Server struct {
	socketPath string
	ln         net.Listener

	mu        sync.RWMutex
	clients   map[*serverClient]struct{}
	history   []LogEntry
	state     *TunnelState
	tunnelURL string

	cmdCh chan Command
}

type serverClient struct {
	conn      net.Conn
	send      chan []byte
	closeOnce sync.Once
}

// NewServer returns an unstarted Server.
func NewServer(socketPath string) *Server {
	return &Server{
		socketPath: socketPath,
		clients:    make(map[*serverClient]struct{}),
		cmdCh:      make(chan Command, 16),
	}
}

// Commands returns the channel of inbound commands.
func (s *Server) Commands() <-chan Command { return s.cmdCh }

// Listen creates the socket and accepts connections.
func (s *Server) Listen() error {
	ln, err := newServerListener(s.socketPath)
	if err != nil {
		return err
	}
	s.ln = ln
	go s.accept()
	return nil
}

// Shutdown stops the listener and disconnects all clients.
func (s *Server) Shutdown() {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.shutdown()
	}
}

func (c *serverClient) shutdown() {
	c.closeOnce.Do(func() { close(c.send) })
}

// SetState replaces the current tunnel state.
func (s *Server) SetState(st TunnelState) {
	s.mu.Lock()
	s.state = &st
	s.mu.Unlock()
}

// BroadcastState updates state and fans the change out.
func (s *Server) BroadcastState(st TunnelState) {
	ev, _ := json.Marshal(Event{Type: TypeState, State: &st})
	ev = append(ev, '\n')

	s.mu.Lock()
	s.state = &st
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

// BroadcastLog appends to history and fans the entry out.
func (s *Server) BroadcastLog(entry LogEntry) {
	ev, _ := json.Marshal(Event{Type: TypeLog, Log: &entry})
	ev = append(ev, '\n')

	s.mu.Lock()
	s.history = append(s.history, entry)
	if len(s.history) > maxHistory {
		s.history = s.history[1:]
	}
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

// BroadcastTunnelURL stores the new public URL and fans it out.
func (s *Server) BroadcastTunnelURL(url string) {
	ev, _ := json.Marshal(Event{Type: TypeTunnel, TunnelURL: url})
	ev = append(ev, '\n')

	s.mu.Lock()
	s.tunnelURL = url
	if s.state != nil {
		s.state.PublicURL = url
	}
	clients := s.clientSlice()
	s.mu.Unlock()

	s.fanOut(clients, ev)
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	c := &serverClient{conn: conn, send: make(chan []byte, 64)}

	s.mu.Lock()
	s.clients[c] = struct{}{}
	snap := s.snapshot()
	s.mu.Unlock()

	if data, err := json.Marshal(snap); err == nil {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.Write(append(data, '\n'))
		conn.SetWriteDeadline(time.Time{})
	}

	go c.writeLoop()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<16), 1<<16)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == TypeCommand && ev.Cmd != nil {
			select {
			case s.cmdCh <- *ev.Cmd:
			default:
				log.Printf("[ipc] command queue full, dropping command %q", ev.Cmd.Action)
			}
			ack, _ := json.Marshal(Event{Type: TypeAck, Ack: "ok"})
			conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			conn.Write(append(ack, '\n'))
			conn.SetWriteDeadline(time.Time{})
		}
	}

	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.shutdown()
}

func (c *serverClient) writeLoop() {
	defer c.conn.Close()
	for data := range c.send {
		c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.conn.Write(data); err != nil {
			c.shutdown()
			for range c.send {
			}
			return
		}
	}
}

func (s *Server) fanOut(clients []*serverClient, data []byte) {
	for _, c := range clients {
		select {
		case c.send <- data:
		default:
			c.shutdown()
		}
	}
}

func (s *Server) clientSlice() []*serverClient {
	out := make([]*serverClient, 0, len(s.clients))
	for c := range s.clients {
		out = append(out, c)
	}
	return out
}

// snapshot returns a deep-copied event for sending to a new client.
// The state is value-copied so the marshaller (which runs after s.mu is
// released) cannot race with concurrent BroadcastState/BroadcastTunnelURL
// callers mutating fields on the original *TunnelState.
func (s *Server) snapshot() Event {
	hist := make([]LogEntry, len(s.history))
	copy(hist, s.history)
	var stateCopy *TunnelState
	if s.state != nil {
		sc := *s.state
		stateCopy = &sc
	}
	return Event{Type: TypeSnapshot, Snapshot: stateCopy, RecentLogs: hist, TunnelURL: s.tunnelURL}
}
