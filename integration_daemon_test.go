//go:build daemon

package tunnel_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/tunnel/internal/ipc"
	"github.com/anivaryam/tunnel/internal/paths"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	name := "tunnel-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-tags", "daemon", "-o", bin, "./cmd/tunnel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestDaemonLifecycle(t *testing.T) {
	bin := buildBinary(t)
	name := "test-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "http", "9", "--silent", "--name", name,
		"--server", "ws://127.0.0.1:1").CombinedOutput()
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	defer exec.Command(bin, "stop", "--name", name).Run()

	sock := paths.Socket(paths.HashFromName(name))
	deadline := time.Now().Add(3 * time.Second)
	var c *ipc.Client
	for time.Now().Before(deadline) {
		c, err = ipc.Dial(sock)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c == nil {
		t.Fatalf("never connected to daemon socket: %v", err)
	}
	defer c.Close()

	ev, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != ipc.TypeSnapshot {
		t.Fatalf("first event = %s; want snapshot", ev.Type)
	}

	out, err = exec.Command(bin, "stop", "--name", name).CombinedOutput()
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "stopped daemon") {
		t.Fatalf("unexpected stop output: %s", out)
	}
}
