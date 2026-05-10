//go:build daemon

// Package monitor implements an interactive TUI attached to a running daemon.
package monitor

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anivaryam/tunnel/internal/ipc"
	"golang.org/x/term"
)

const (
	maxLogs   = 5000
	clearScrn = "\033[2J\033[H"
	hideCur   = "\033[?25l"
	showCur   = "\033[?25h"
)

// Run connects to the daemon at socketPath and renders the TUI until the
// user presses 'q' or the daemon disconnects.
func Run(socketPath string) error {
	c, err := dialWithRetry(socketPath)
	if err != nil {
		return err
	}
	defer c.Close()

	st, oldState, err := enterRaw()
	if err != nil {
		return err
	}
	defer leaveRaw(st, oldState)

	m := &model{
		client: c,
		logs:   make([]ipc.LogEntry, 0, 256),
	}

	// Reader goroutine.
	go func() {
		for {
			ev, err := c.Recv()
			if err != nil {
				m.mu.Lock()
				m.disconnected = true
				m.mu.Unlock()
				return
			}
			m.apply(ev)
		}
	}()

	// Keystroke goroutine.
	keyCh := make(chan byte, 8)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			keyCh <- buf[0]
		}
	}()

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		m.render()
		select {
		case k := <-keyCh:
			if k == 'q' || k == 0x03 /* ctrl-c */ {
				return nil
			}
		case <-tick.C:
			m.mu.RLock()
			done := m.disconnected
			m.mu.RUnlock()
			if done {
				m.render()
				return nil
			}
		}
	}
}

type model struct {
	mu           sync.RWMutex
	client       *ipc.Client
	state        ipc.TunnelState
	url          string
	logs         []ipc.LogEntry
	disconnected bool
}

func (m *model) apply(ev ipc.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev.Type {
	case ipc.TypeSnapshot:
		if ev.Snapshot != nil {
			m.state = *ev.Snapshot
		}
		m.url = ev.TunnelURL
		if len(ev.RecentLogs) > 0 {
			m.logs = append(m.logs, ev.RecentLogs...)
		}
	case ipc.TypeState:
		if ev.State != nil {
			m.state = *ev.State
		}
	case ipc.TypeTunnel:
		m.url = ev.TunnelURL
	case ipc.TypeLog:
		if ev.Log != nil {
			m.logs = append(m.logs, *ev.Log)
			if len(m.logs) > maxLogs {
				m.logs = m.logs[len(m.logs)-maxLogs:]
			}
		}
	}
}

func (m *model) render() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	w, h, _ := term.GetSize(int(os.Stdout.Fd()))
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	var b strings.Builder
	b.WriteString(clearScrn)
	fmt.Fprintf(&b, "tunnel monitor   q=quit\r\n")
	fmt.Fprintf(&b, "name: %-20s  state: %-12s  pid: %d\r\n",
		safe(m.state.Name), safe(m.state.State), m.state.PID)
	if m.url != "" {
		fmt.Fprintf(&b, "public: %s\r\n", m.url)
	}
	fmt.Fprintf(&b, "local:  %s://localhost:%d   reconnects: %d\r\n",
		safe(m.state.Mode), m.state.LocalPort, m.state.Reconnects)
	if !m.state.StartedAt.IsZero() {
		fmt.Fprintf(&b, "uptime: %s\r\n", time.Since(m.state.StartedAt).Round(time.Second))
	}
	if m.disconnected {
		fmt.Fprintf(&b, "\033[31mDISCONNECTED — daemon exited or socket closed\033[0m\r\n")
	}
	b.WriteString(strings.Repeat("─", w) + "\r\n")
	b.WriteString("LOGS\r\n")

	// Print last (h - header) log lines.
	header := 8
	logLines := h - header
	if logLines < 1 {
		logLines = 1
	}
	start := len(m.logs) - logLines
	if start < 0 {
		start = 0
	}
	for _, e := range m.logs[start:] {
		line := e.Line
		if len(line) > w {
			line = line[:w]
		}
		if e.IsError {
			fmt.Fprintf(&b, "\033[31m%s\033[0m\r\n", line)
		} else if e.IsEvent {
			fmt.Fprintf(&b, "\033[33m%s\033[0m\r\n", line)
		} else {
			fmt.Fprintf(&b, "%s\r\n", line)
		}
	}
	os.Stdout.WriteString(b.String())
}

func safe(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func dialWithRetry(socketPath string) (*ipc.Client, error) {
	var last error
	for i := 0; i < 10; i++ {
		c, err := ipc.Dial(socketPath)
		if err == nil {
			return c, nil
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, last
}
