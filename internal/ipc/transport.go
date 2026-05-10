//go:build daemon

package ipc

import (
	"net"
	"time"
)

func newServerListener(addr string) (net.Listener, error) {
	if err := removeStale(addr); err != nil {
		return nil, err
	}
	return platformListen(addr)
}

func dialAddr(addr string, timeout time.Duration) (net.Conn, error) {
	return platformDial(addr, timeout)
}
