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

	srv2 := NewServer(addr)
	if err := srv2.Listen(); err != nil {
		t.Fatalf("relisten: %v", err)
	}
	srv2.Shutdown()

	_ = time.Now()
}
