//go:build daemon

// Package daemon handles re-execing the tunnel binary detached from the
// terminal and managing PID/socket state files.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// EnvDaemonChild is set on re-exec; the child runs the foreground client loop
// but registers an IPC server because of --silent (which it still receives).
const EnvDaemonChild = "TUNNEL_DAEMON_CHILD"

type pidFile struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

// Reexec re-execs the current binary with args, fully detached. The child is
// responsible for setting up its own stdout/stderr (parent passes the log path
// via env vars so the child can install a rotating writer if requested).
func Reexec(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("cannot find executable: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = append(os.Environ(), EnvDaemonChild+"=1")
	setDetachAttr(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	return pid, nil
}

// WritePID writes pid+addr to pidPath as JSON.
func WritePID(pidPath string, pid int, addr string) error {
	pf := pidFile{PID: pid, Addr: addr}
	data, err := json.Marshal(pf)
	if err != nil {
		return err
	}
	return os.WriteFile(pidPath, data, 0600)
}

// ReadPID returns pid + addr stored in pidPath.
func ReadPID(pidPath string) (int, string, error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, "", err
	}
	var pf pidFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return 0, "", fmt.Errorf("invalid PID file %s: %w", pidPath, err)
	}
	return pf.PID, pf.Addr, nil
}

// Cleanup removes the PID and socket files (best effort).
func Cleanup(pidPath, socketPath string) {
	os.Remove(pidPath)
	os.Remove(socketPath)
}

// StripFlag removes a flag (and its value if hasValue) from args.
// Handles three forms: "--flag", "--flag value", and "--flag=value".
func StripFlag(args []string, flag string, hasValue bool) []string {
	out := make([]string, 0, len(args))
	skip := false
	prefix := flag + "="
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == flag {
			if hasValue {
				skip = true
			}
			continue
		}
		if hasValue && strings.HasPrefix(a, prefix) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// IsChild reports whether the current process is the detached daemon child.
func IsChild() bool { return os.Getenv(EnvDaemonChild) == "1" }
