// Package paths derives stable per-instance file paths for tunnel daemons.
package paths

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// HashFromName returns an 8-char hash from a tunnel name.
func HashFromName(name string) string {
	sum := sha256.Sum256([]byte("name:" + name))
	return fmt.Sprintf("%x", sum)[:8]
}

// HashFromMode returns an 8-char hash for an unnamed tunnel keyed by mode+port.
func HashFromMode(mode string, port int) string {
	sum := sha256.Sum256([]byte("mode:" + mode + ":" + strconv.Itoa(port)))
	return fmt.Sprintf("%x", sum)[:8]
}

// RuntimeDir returns a per-user, mode-0700 directory used to host the IPC
// socket, PID, and (default) log files. On Linux it prefers XDG_RUNTIME_DIR.
// Falls back to a uid-namespaced subdirectory under os.TempDir() so multiple
// users on the same host never share a directory. The directory is created
// on first call.
func RuntimeDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
		}
		dir := filepath.Join(base, "tunnel")
		_ = os.MkdirAll(dir, 0700)
		return dir
	}
	uid := strconv.Itoa(os.Getuid())
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dir := filepath.Join(xdg, "tunnel")
		if err := os.MkdirAll(dir, 0700); err == nil {
			_ = os.Chmod(dir, 0700)
			return dir
		}
	}
	dir := filepath.Join(os.TempDir(), "tunnel-"+uid)
	_ = os.MkdirAll(dir, 0700)
	_ = os.Chmod(dir, 0700)
	return dir
}

// Socket returns the IPC socket (Unix) or named pipe (Windows) path.
// Windows pipes are namespaced by uid via LOCALAPPDATA-derived RuntimeDir
// for the PID/log; the pipe itself uses a fixed prefix that the OS scopes
// per-session.
func Socket(hash string) string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\tunnel-` + hash
	}
	return filepath.Join(RuntimeDir(), "tunnel-"+hash+".sock")
}

// PID returns the PID file path under the per-user runtime dir (0700).
func PID(hash string) string {
	return filepath.Join(RuntimeDir(), "tunnel-"+hash+".pid")
}

// Log returns the default log file path under the per-user runtime dir.
func Log(hash string) string {
	return filepath.Join(RuntimeDir(), "tunnel-"+hash+".log")
}
