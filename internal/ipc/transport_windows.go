//go:build daemon && windows

package ipc

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

func removeStale(addr string) error { return nil }

func platformListen(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, nil)
}

func platformDial(addr string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return winio.DialPipeContext(ctx, addr)
}
