//go:build daemon

package ipc

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServerSnapshotAndBroadcast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix socket path")
	}
	sock := filepath.Join(t.TempDir(), "t.sock")

	srv := NewServer(sock)
	srv.SetState(TunnelState{Name: "x", State: "connecting", LocalPort: 8080})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// First message must be the snapshot.
	ev, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != TypeSnapshot || ev.Snapshot == nil || ev.Snapshot.Name != "x" {
		t.Fatalf("snapshot mismatch: %+v", ev)
	}

	// Now broadcast a log line and confirm the client receives it.
	srv.BroadcastLog(LogEntry{At: time.Now(), Line: "hello"})
	ev, err = c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != TypeLog || ev.Log == nil || ev.Log.Line != "hello" {
		t.Fatalf("log mismatch: %+v", ev)
	}
}

func TestServerReceivesCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix socket path")
	}
	sock := filepath.Join(t.TempDir(), "t.sock")
	srv := NewServer(sock)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Drain snapshot.
	if _, err := c.Recv(); err != nil {
		t.Fatal(err)
	}

	if err := c.Send(Command{Action: "stop"}); err != nil {
		t.Fatal(err)
	}

	select {
	case cmd := <-srv.Commands():
		if cmd.Action != "stop" {
			t.Fatalf("got %+v", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received command")
	}
}
