package paths

import (
	"runtime"
	"strings"
	"testing"
)

func TestHashFromName(t *testing.T) {
	h := HashFromName("eservice")
	if len(h) != 8 {
		t.Fatalf("hash len = %d, want 8", len(h))
	}
	if h != HashFromName("eservice") {
		t.Fatalf("hash unstable")
	}
	if h == HashFromName("eservice2") {
		t.Fatalf("hash collision")
	}
}

func TestHashFromMode(t *testing.T) {
	if HashFromMode("http", 8080) == HashFromMode("tcp", 8080) {
		t.Fatalf("mode hashes should differ")
	}
}

func TestSocketPath(t *testing.T) {
	p := Socket("abc12345")
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(p, `\\.\pipe\tunnel-`) {
			t.Fatalf("windows socket: %s", p)
		}
	} else {
		if !strings.Contains(p, "tunnel-abc12345.sock") {
			t.Fatalf("unix socket: %s", p)
		}
	}
}

func TestPIDPath(t *testing.T) {
	if !strings.Contains(PID("abc12345"), "tunnel-abc12345.pid") {
		t.Fatal("pid path missing prefix")
	}
}

func TestLogPath(t *testing.T) {
	if !strings.Contains(Log("abc12345"), "tunnel-abc12345.log") {
		t.Fatal("log path missing prefix")
	}
}
