//go:build daemon

package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client is an IPC client connected to a running daemon.
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
}

// Dial connects to the daemon socket.
func Dial(socketPath string) (*Client, error) {
	conn, err := dialAddr(socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no tunnel daemon running (socket: %s)", socketPath)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &Client{conn: conn, scanner: sc}, nil
}

// Recv blocks until the next event arrives.
func (c *Client) Recv() (Event, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Event{}, err
		}
		return Event{}, fmt.Errorf("daemon disconnected")
	}
	var ev Event
	if err := json.Unmarshal(c.scanner.Bytes(), &ev); err != nil {
		return Event{}, fmt.Errorf("bad event: %w", err)
	}
	return ev, nil
}

// Send sends a command to the daemon.
func (c *Client) Send(cmd Command) error {
	ev := Event{Type: TypeCommand, Cmd: &cmd}
	data, _ := json.Marshal(ev)
	_, err := c.conn.Write(append(data, '\n'))
	return err
}

// Close shuts the connection.
func (c *Client) Close() { c.conn.Close() }
