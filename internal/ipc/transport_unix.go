//go:build daemon && !windows

package ipc

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

func ownedByCurrentUID(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}

// removeStale removes a leftover socket from a prior daemon, but refuses to
// touch the path unless it is a Unix socket owned by the current uid. This
// prevents a hostile local user from pre-creating the path with a regular
// file (or symlink) and tricking the daemon into deleting unrelated files
// or hijacking the socket.
func removeStale(addr string) error {
	info, err := os.Lstat(addr)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove %q: not a socket (mode=%v)", addr, info.Mode())
	}
	if !ownedByCurrentUID(info) {
		return fmt.Errorf("refusing to remove %q: not owned by current user", addr)
	}
	return os.Remove(addr)
}

func platformListen(addr string) (net.Listener, error) {
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	// Ensure the socket itself is mode 0600 — only the owning user may
	// connect. Without this, anyone with execute on the parent directory
	// could connect and issue stop/snapshot commands. The parent dir is
	// already 0700 (paths.RuntimeDir), but defence-in-depth.
	if cerr := os.Chmod(addr, 0600); cerr != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", cerr)
	}
	return ln, nil
}

func platformDial(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}
