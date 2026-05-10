//go:build daemon

package client

import (
	"sync"
	"time"

	"github.com/anivaryam/tunnel/internal/ipc"
)

// IPCBridge adapts an *ipc.Server to the EventSink interface. It keeps a local
// copy of the current TunnelState so that partial updates (state, URL) can be
// merged into a full snapshot and broadcast as a TypeState event.
//
// The daemon entrypoint MUST call SetInitial() before passing the bridge to
// Client.Sink so the first BroadcastState carries identity fields (Name, Mode,
// LocalPort, PID, StartedAt).
type IPCBridge struct {
	Srv *ipc.Server

	mu    sync.Mutex
	state ipc.TunnelState
}

// SetInitial seeds identity fields. Call once before the client starts.
func (b *IPCBridge) SetInitial(s ipc.TunnelState) {
	b.mu.Lock()
	b.state = s
	snap := b.state
	b.mu.Unlock()
	b.Srv.SetState(snap)
}

func (b *IPCBridge) OnLog(line string, isError bool) {
	b.Srv.BroadcastLog(ipc.LogEntry{At: time.Now(), Line: line, IsError: isError})
}

func (b *IPCBridge) OnTunnelURL(url string) {
	b.mu.Lock()
	b.state.PublicURL = url
	snap := b.state
	b.mu.Unlock()
	b.Srv.BroadcastTunnelURL(url)
	b.Srv.BroadcastState(snap)
}

func (b *IPCBridge) OnState(state string) {
	b.mu.Lock()
	if state == "reconnecting" {
		b.state.Reconnects++
	}
	b.state.State = state
	snap := b.state
	b.mu.Unlock()
	b.Srv.BroadcastState(snap)
}
