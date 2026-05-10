//go:build daemon

package daemon

import (
	"path/filepath"
	"testing"
)

func TestPIDFileRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pid")
	if err := WritePID(p, 4242, "/tmp/x.sock"); err != nil {
		t.Fatal(err)
	}
	pid, addr, err := ReadPID(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4242 || addr != "/tmp/x.sock" {
		t.Fatalf("got pid=%d addr=%s", pid, addr)
	}
}

func TestStripFlag(t *testing.T) {
	in := []string{"http", "8080", "--silent", "--log-file", "/tmp/x.log", "--name", "foo"}
	out := StripFlag(in, "--silent", false)
	out = StripFlag(out, "--log-file", true)
	want := []string{"http", "8080", "--name", "foo"}
	if len(out) != len(want) {
		t.Fatalf("got %v", out)
	}
	for i := range out {
		if out[i] != want[i] {
			t.Fatalf("got %v", out)
		}
	}
}

func TestStripFlagEqualsForm(t *testing.T) {
	in := []string{"http", "8080", "--log-file=/tmp/x.log", "--name=foo", "--max-log-size=1024"}
	out := StripFlag(in, "--log-file", true)
	out = StripFlag(out, "--max-log-size", true)
	want := []string{"http", "8080", "--name=foo"}
	if len(out) != len(want) {
		t.Fatalf("got %v want %v", out, want)
	}
	for i := range out {
		if out[i] != want[i] {
			t.Fatalf("got %v want %v", out, want)
		}
	}
}
