//go:build daemon

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/anivaryam/tunnel/internal/client"
	"github.com/anivaryam/tunnel/internal/daemon"
	"github.com/anivaryam/tunnel/internal/ipc"
	"github.com/anivaryam/tunnel/internal/logrotate"
	"github.com/anivaryam/tunnel/internal/monitor"
	"github.com/anivaryam/tunnel/internal/paths"
	"github.com/spf13/cobra"
)

// daemonTarget formats a user-friendly description of which daemon a
// command is targeting (--name X or --port N --mode M). Used in error
// messages instead of raw filesystem paths or hashes.
func daemonTarget(name string, port int, mode string) string {
	if name != "" {
		return fmt.Sprintf("name=%q", name)
	}
	if mode == "" {
		mode = "http"
	}
	return fmt.Sprintf("port=%d mode=%s", port, mode)
}

type daemonFlagSet struct {
	silent     bool
	logFile    string
	maxLogSize int64
}

func registerDaemonFlags(cmd *cobra.Command, f *daemonFlagSet) {
	cmd.Flags().BoolVarP(&f.silent, "silent", "s", false, "run in the background and exit (daemonize)")
	cmd.Flags().StringVar(&f.logFile, "log-file", "", "write daemon output to this file (default: $TMPDIR/tunnel-<hash>.log)")
	cmd.Flags().Int64Var(&f.maxLogSize, "max-log-size", 0, "rotate log file when it exceeds this size in bytes (0 = no rotation)")
}

func registerDaemonCommands(root *cobra.Command, _ *daemonFlagSet) {
	var nameFlag string
	var portFlag int
	var modeFlag string

	resolveHash := func() (string, error) {
		if nameFlag != "" {
			return paths.HashFromName(nameFlag), nil
		}
		if portFlag == 0 {
			return "", fmt.Errorf("specify --name or --port to identify the tunnel")
		}
		mode := modeFlag
		if mode == "" {
			mode = "http"
		}
		return paths.HashFromMode(mode, portFlag), nil
	}

	stopCmd := &cobra.Command{
		Use:     "stop",
		Short:   "Stop a running daemon (--name or --port required)",
		Example: "  tunnel stop --name api\n  tunnel stop --port 3000 --mode http",
		RunE: func(_ *cobra.Command, _ []string) error {
			hash, err := resolveHash()
			if err != nil {
				return err
			}
			target := daemonTarget(nameFlag, portFlag, modeFlag)
			pidPath := paths.PID(hash)
			pid, _, err := daemon.ReadPID(pidPath)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("no running daemon for %s", target)
				}
				return fmt.Errorf("read PID file for %s: %w", target, err)
			}
			if !daemon.IsAlive(pid) {
				daemon.Cleanup(pidPath, paths.Socket(hash))
				return fmt.Errorf("daemon for %s (pid=%d) not running; cleaned up stale files", target, pid)
			}
			if err := daemon.StopProcess(pid); err != nil {
				return fmt.Errorf("stop %s pid=%d: %w", target, pid, err)
			}
			daemon.Cleanup(pidPath, paths.Socket(hash))
			fmt.Printf("stopped daemon for %s (pid=%d)\n", target, pid)
			return nil
		},
	}
	stopCmd.Flags().StringVar(&nameFlag, "name", "", "tunnel name")
	stopCmd.Flags().IntVar(&portFlag, "port", 0, "local port (when no name was used)")
	stopCmd.Flags().StringVar(&modeFlag, "mode", "http", "tunnel mode (http|tcp|udp) — only with --port")

	var jsonFlag bool
	statusCmd := &cobra.Command{
		Use:     "status",
		Short:   "Show daemon status (--name or --port required)",
		Example: "  tunnel status --name api\n  tunnel status --name api --json",
		RunE: func(_ *cobra.Command, _ []string) error {
			hash, err := resolveHash()
			if err != nil {
				return err
			}
			target := daemonTarget(nameFlag, portFlag, modeFlag)
			c, err := ipc.Dial(paths.Socket(hash))
			if err != nil {
				return fmt.Errorf("no running daemon for %s", target)
			}
			defer c.Close()
			// Snapshots stream every ~250ms. The first one immediately after
			// daemonize often shows state="connecting" before the assignment
			// is received. Read up to a few snapshots to surface the most
			// useful state without making the user re-run.
			var ev ipc.Event
			deadline := time.Now().Add(800 * time.Millisecond)
			for {
				e, rErr := c.Recv()
				if rErr != nil {
					if ev.Snapshot != nil {
						break
					}
					return fmt.Errorf("read snapshot for %s: %w", target, rErr)
				}
				ev = e
				if ev.Snapshot != nil && ev.Snapshot.State != "connecting" {
					break
				}
				if time.Now().After(deadline) {
					break
				}
			}
			if ev.Snapshot == nil {
				return fmt.Errorf("no snapshot received from daemon for %s", target)
			}
			s := ev.Snapshot
			if jsonFlag {
				out := map[string]any{
					"name":       s.Name,
					"mode":       s.Mode,
					"local_port": s.LocalPort,
					"state":      s.State,
					"public_url": ev.TunnelURL,
					"pid":        s.PID,
					"started_at": s.StartedAt.Format(time.RFC3339),
					"reconnects": s.Reconnects,
				}
				return json.NewEncoder(os.Stdout).Encode(out)
			}
			fmt.Printf("name:        %s\n", s.Name)
			fmt.Printf("mode:        %s\n", s.Mode)
			fmt.Printf("local port:  %d\n", s.LocalPort)
			fmt.Printf("state:       %s\n", s.State)
			fmt.Printf("public url:  %s\n", ev.TunnelURL)
			fmt.Printf("pid:         %d\n", s.PID)
			fmt.Printf("started at:  %s\n", s.StartedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("reconnects:  %d\n", s.Reconnects)
			return nil
		},
	}
	statusCmd.Flags().StringVar(&nameFlag, "name", "", "tunnel name")
	statusCmd.Flags().IntVar(&portFlag, "port", 0, "local port")
	statusCmd.Flags().StringVar(&modeFlag, "mode", "http", "tunnel mode")
	statusCmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")

	monitorCmd := &cobra.Command{
		Use:     "monitor",
		Short:   "Open an interactive TUI attached to a running daemon",
		Example: "  tunnel monitor --name api",
		RunE: func(_ *cobra.Command, _ []string) error {
			hash, err := resolveHash()
			if err != nil {
				return err
			}
			target := daemonTarget(nameFlag, portFlag, modeFlag)
			// Fail fast if the socket isn't there — the TUI would otherwise
			// hang on connect for the full IPC dial timeout.
			if _, statErr := os.Stat(paths.Socket(hash)); os.IsNotExist(statErr) {
				return fmt.Errorf("no running daemon for %s", target)
			}
			return monitor.Run(paths.Socket(hash))
		},
	}
	monitorCmd.Flags().StringVar(&nameFlag, "name", "", "tunnel name")
	monitorCmd.Flags().IntVar(&portFlag, "port", 0, "local port")
	monitorCmd.Flags().StringVar(&modeFlag, "mode", "http", "tunnel mode")

	// Group under `tunnel daemon ...` for cleaner discoverability.
	// Top-level `tunnel stop|status|monitor` remain registered for
	// backward compatibility with existing scripts and shell history.
	daemonGroup := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon subcommands (stop|status|monitor)",
	}
	groupStop := *stopCmd
	groupStatus := *statusCmd
	groupMonitor := *monitorCmd
	daemonGroup.AddCommand(&groupStop, &groupStatus, &groupMonitor)

	root.AddCommand(stopCmd, statusCmd, monitorCmd, daemonGroup)
}

// instanceHash picks a stable hash for this invocation.
func instanceHash(c *client.Client, mode string) string {
	if c.Name != "" {
		return paths.HashFromName(c.Name)
	}
	return paths.HashFromMode(mode, c.LocalPort)
}

// resolvedLogPath returns the user-supplied path or the default in $TMPDIR.
func resolvedLogPath(f daemonFlagSet, hash string) string {
	if f.logFile != "" {
		return f.logFile
	}
	return paths.Log(hash)
}

// maybeDaemonize: if --silent and we are NOT the child, re-exec and exit.
// Otherwise no-op. Errors propagate.
func maybeDaemonize(f daemonFlagSet, argv []string, c *client.Client, mode string) error {
	if !f.silent || daemon.IsChild() {
		return nil
	}

	hash := instanceHash(c, mode)
	pidPath := paths.PID(hash)

	// Serialize the check→reexec→write-pid window with an O_EXCL lock file so two
	// concurrent `tunnel http` invocations for the same hash can't both spawn a
	// daemon (TOCTOU on the pid file). ponytail: a crash between create and
	// remove leaves a stale .lock that blocks daemonize until deleted manually;
	// upgrade to flock with liveness if that proves painful.
	lockPath := pidPath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("another tunnel daemonize is in progress (hash=%s); if none is, remove %s", hash, lockPath)
		}
		return fmt.Errorf("acquire daemon lock: %w", err)
	}
	lf.Close()
	defer os.Remove(lockPath)

	if pid, _, err := daemon.ReadPID(pidPath); err == nil && daemon.IsAlive(pid) {
		return fmt.Errorf("tunnel daemon already running (pid=%d, hash=%s); use 'tunnel stop' first", pid, hash)
	}

	logPath := resolvedLogPath(f, hash)
	args := daemon.StripFlag(argv[1:], "--silent", false)
	args = daemon.StripFlag(args, "-s", false)
	args = daemon.StripFlag(args, "--log-file", true)
	args = daemon.StripFlag(args, "--max-log-size", true)

	os.Setenv("TUNNEL_DAEMON_LOG", logPath)
	if f.maxLogSize > 0 {
		os.Setenv("TUNNEL_DAEMON_LOG_MAX", strconv.FormatInt(f.maxLogSize, 10))
	}
	pid, err := daemon.Reexec(args)
	if err != nil {
		return err
	}
	if err := daemon.WritePID(pidPath, pid, paths.Socket(hash)); err != nil {
		return fmt.Errorf("write PID file: %w", err)
	}
	// Wait up to 3s for the child to bring the IPC socket up AND publish a
	// snapshot transitioning out of "connecting" — otherwise daemonize may
	// return success while the child silently fails to reach the relay
	// (bad URL, bad token, network down). Best-effort: any error here only
	// downgrades to a warning so users still see the PID/socket lines.
	verifyMsg := waitForDaemonReady(paths.Socket(hash), 3*time.Second)
	fmt.Printf("tunnel daemonized (pid=%d, hash=%s)\n", pid, hash)
	fmt.Printf("logs:   %s\n", logPath)
	fmt.Printf("socket: %s\n", paths.Socket(hash))
	fmt.Printf("monitor: tunnel monitor%s\n", monitorArgs(c))
	if verifyMsg != "" {
		fmt.Fprintln(os.Stderr, "warning:", verifyMsg)
	}
	return nil
}

// waitForDaemonReady polls the child's IPC socket until a snapshot reports
// state != "connecting", or the deadline expires. Returns "" on success or
// a short user-readable diagnostic string. Verbose logs go through tail
// of the daemon log file when --debug is set.
func waitForDaemonReady(socketPath string, total time.Duration) string {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(socketPath)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		ev, err := c.Recv()
		c.Close()
		if err != nil || ev.Snapshot == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		switch ev.Snapshot.State {
		case "connected":
			return ""
		case "exited":
			return "daemon exited before becoming ready — check the log file"
		case "":
			// Wait for a real state.
		default:
			// "connecting" or "reconnecting" — keep polling.
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "daemon did not reach 'connected' state within " + total.String() + " — check the log file or run `tunnel status`"
}

func monitorArgs(c *client.Client) string {
	var modeSuffix string
	if c.Mode != "" && c.Mode != "http" {
		modeSuffix = " --mode " + c.Mode
	}
	if c.Name != "" {
		return " --name " + c.Name + modeSuffix
	}
	return " --port " + strconv.Itoa(c.LocalPort) + modeSuffix
}

// shouldExitParent: in --silent mode, the parent process exits as soon as the
// child has been spawned. The child runs through normally.
func shouldExitParent(f daemonFlagSet) bool {
	return f.silent && !daemon.IsChild()
}

// daemonLogState owns shutdown wiring for the rotating-log pump goroutine.
// Close() flushes buffered stdout/stderr writes to the rotating writer and
// then closes the rotating writer's underlying file.
type daemonLogState struct {
	pumpDone chan struct{}
	pw       *os.File
	rw       *logrotate.Writer
}

var activeLogState atomic.Pointer[daemonLogState]

// setupChildLogging redirects os.Stdout/Stderr to logPath. If maxSize > 0 a
// pipe-backed logrotate.Writer is installed so writes go through size-based
// rotation. maxSize == 0 just opens the file in append mode.
func setupChildLogging(logPath string, maxSize int64) error {
	if maxSize == 0 {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		os.Stdout = f
		os.Stderr = f
		return nil
	}
	rw, err := logrotate.New(logPath, maxSize, 3)
	if err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		rw.Close()
		return err
	}
	state := &daemonLogState{
		pumpDone: make(chan struct{}),
		pw:       pw,
		rw:       rw,
	}
	go func() {
		defer close(state.pumpDone)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				_, _ = rw.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	os.Stdout = pw
	os.Stderr = pw
	activeLogState.Store(state)
	return nil
}

// shutdownChildLogging flushes the daemon log pipe before the process exits.
// Closes the write end so the reader goroutine sees EOF, waits for it to
// drain, then closes the rotating writer. Safe to call when no rotating log
// is installed (no-op).
func shutdownChildLogging() {
	state := activeLogState.Swap(nil)
	if state == nil {
		return
	}
	_ = state.pw.Close()
	select {
	case <-state.pumpDone:
	case <-time.After(2 * time.Second):
	}
	_ = state.rw.Close()
}

// attachIPC starts the IPC server in the daemon child, seeds initial state,
// and wires the EventSink.
func attachIPC(c *client.Client, f daemonFlagSet) {
	if !daemon.IsChild() {
		return
	}
	// Child-only: install daemon log file (rotating if requested).
	if logPath := os.Getenv("TUNNEL_DAEMON_LOG"); logPath != "" {
		var maxSize int64
		if v := os.Getenv("TUNNEL_DAEMON_LOG_MAX"); v != "" {
			maxSize, _ = strconv.ParseInt(v, 10, 64)
		}
		if err := setupChildLogging(logPath, maxSize); err != nil {
			// best effort — fall back to inherited (likely closed) stdio.
			fmt.Fprintf(os.Stderr, "log setup failed: %v\n", err)
		}
	}

	mode := c.Mode
	if mode == "" {
		mode = "http"
	}
	hash := instanceHashForChild(c)
	srv := ipc.NewServer(paths.Socket(hash))
	if err := srv.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "ipc listen failed: %v\n", err)
		daemon.Cleanup(paths.PID(hash), paths.Socket(hash))
		os.Exit(1)
	}
	bridge := &client.IPCBridge{Srv: srv}
	bridge.SetInitial(ipc.TunnelState{
		Name:      c.Name,
		Mode:      mode,
		LocalPort: c.LocalPort,
		State:     "connecting",
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	})
	c.Sink = bridge

	go func() {
		for cmd := range srv.Commands() {
			if cmd.Action == "stop" {
				p, _ := os.FindProcess(os.Getpid())
				_ = p.Signal(os.Interrupt)
				return
			}
		}
	}()
}

func instanceHashForChild(c *client.Client) string {
	mode := c.Mode
	if mode == "" {
		mode = "http"
	}
	return instanceHash(c, mode)
}
