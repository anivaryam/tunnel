//go:build daemon

// Package ipc defines the wire format for the tunnel daemon socket.
//
// Messages are newline-delimited JSON. The daemon (server) broadcasts
// snapshot, log, state, and tunnel events; clients send command events.
package ipc

import "time"

const (
	TypeSnapshot = "snapshot"
	TypeLog      = "log"
	TypeState    = "state"
	TypeTunnel   = "tunnel"
	TypeCommand  = "command"
	TypeAck      = "ack"
)

// TunnelState is the current connection state advertised by the daemon.
type TunnelState struct {
	Name       string    `json:"name"` // requested name or auto-generated id
	Mode       string    `json:"mode"` // http | tcp | udp
	LocalPort  int       `json:"local_port"`
	State      string    `json:"state"` // connecting | connected | reconnecting | exited
	PublicURL  string    `json:"public_url,omitempty"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	Reconnects int       `json:"reconnects"`
}

// LogEntry is a single line of daemon output (request log, banner, error, etc.).
type LogEntry struct {
	At      time.Time `json:"at"`
	Line    string    `json:"line"`
	IsEvent bool      `json:"is_event,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
}

// Command is sent from a CLI client to the daemon.
type Command struct {
	Action string `json:"action"` // "stop" | "reconnect"
}

// Event is the envelope for all messages.
type Event struct {
	Type string `json:"type"`

	State     *TunnelState `json:"state,omitempty"`
	Log       *LogEntry    `json:"log,omitempty"`
	TunnelURL string       `json:"tunnel_url,omitempty"`

	Snapshot   *TunnelState `json:"snapshot,omitempty"`
	RecentLogs []LogEntry   `json:"recent_logs,omitempty"`

	Cmd *Command `json:"cmd,omitempty"`
	Ack string   `json:"ack,omitempty"`
}
