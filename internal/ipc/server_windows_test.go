//go:build daemon && windows

package ipc

import (
	"testing"
	"time"
)

func TestNamedPipeListenAndDial(t *testing.T) {
	addr := `\\.\pipe\tunnel-test-` + t.Name()

	srv := NewServer(addr)
	srv.SetState(TunnelState{Name: "win", State: "connecting", LocalPort: 80})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	c, err := Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ev, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != TypeSnapshot || ev.Snapshot == nil || ev.Snapshot.Name != "win" {
		t.Fatalf("unexpected snapshot: %+v", ev)
	}
	c.Close()
	srv.Shutdown()

	deadline := time.Now().Add(time.Second)
	var srv2 *Server
	var lastErr error
	for time.Now().Before(deadline) {
		srv2 = NewServer(addr)
		lastErr = srv2.Listen()
		if lastErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("relisten: %v", lastErr)
	}
	srv2.Shutdown()
}
