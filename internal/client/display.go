package client

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-colorable"
	"golang.org/x/term"
)

// ANSI color codes — emitted only when colorEnabled() reports true. Resolve
// once at package init: respect NO_COLOR (https://no-color.org), CLICOLOR=0,
// honor CLICOLOR_FORCE=1, and otherwise gate on stdout being a TTY.
var (
	colorReset  string
	colorGreen  string
	colorYellow string
	colorRed    string
	colorCyan   string
	colorBold   string
	colorDim    string
)

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if v := os.Getenv("CLICOLOR"); v == "0" {
		return false
	}
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func init() {
	if colorEnabled() {
		colorReset = "\033[0m"
		colorGreen = "\033[32m"
		colorYellow = "\033[33m"
		colorRed = "\033[31m"
		colorCyan = "\033[36m"
		colorBold = "\033[1m"
		colorDim = "\033[2m"
	}
}

// colorableOut wraps os.Stdout to handle ANSI color codes on Windows.
var colorableOut = colorable.NewColorableStdout()

// Display handles terminal output for the client.
type Display struct {
	Quiet bool // suppress all output except errors
	sink  EventSink
}

// NewDisplay creates a new Display.
func NewDisplay() *Display {
	return &Display{}
}

// attachSink wires an event sink. Pass nil to detach.
func (d *Display) attachSink(s EventSink) { d.sink = s }

// ---------------------------------------------------------------------------
// Plain-text helpers (no ANSI) — single source of truth for sink lines.
// ---------------------------------------------------------------------------

func plainBanner(publicURL string, localPort int, inspect bool) string {
	if inspect {
		return fmt.Sprintf("tunnel is running — forwarding %s → http://localhost:%d (inspect: http://localhost:4040)", publicURL, localPort)
	}
	return fmt.Sprintf("tunnel is running — forwarding %s → http://localhost:%d", publicURL, localPort)
}

func plainTCPBanner(tcpAddr string, localPort int) string {
	return fmt.Sprintf("tunnel TCP — forwarding %s → localhost:%d", tcpAddr, localPort)
}

func plainUDPBanner(udpAddr string, localPort int) string {
	return fmt.Sprintf("tunnel UDP — forwarding %s → localhost:%d", udpAddr, localPort)
}

func plainRequest(method, path string, status int, elapsed time.Duration) string {
	return fmt.Sprintf("%-7s %s %d %s", method, path, status, elapsed.Round(time.Millisecond))
}

func plainDisconnected(err error) string {
	return fmt.Sprintf("disconnected: %v", err)
}

func plainReconnecting(backoff time.Duration) string {
	return fmt.Sprintf("reconnecting in %s...", backoff)
}

func plainReconnected(publicURL string) string {
	return fmt.Sprintf("reconnected: %s", publicURL)
}

func plainTCPStream(streamID, remoteAddr string, opened bool) string {
	tag := "stream open"
	if !opened {
		tag = "stream closed"
	}
	return fmt.Sprintf("%s %s %s", tag, remoteAddr, streamID[:8])
}

// ---------------------------------------------------------------------------
// Print* methods
// ---------------------------------------------------------------------------

// PrintBanner prints the tunnel connection banner.
func (d *Display) PrintBanner(publicURL string, localPort int, inspect bool) {
	plainLine := plainBanner(publicURL, localPort, inspect)
	if d.sink != nil {
		d.sink.OnTunnelURL(publicURL)
		d.sink.OnLog(plainLine, false)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "%s%stunnel%s %sis running%s\n", colorBold, colorCyan, colorReset, colorGreen, colorReset)
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  Forwarding: %s%s%s → %shttp://localhost:%d%s\n",
		colorBold, publicURL, colorReset,
		colorDim, localPort, colorReset)
	if inspect {
		fmt.Fprintf(colorableOut, "  Inspect:    %shttp://localhost:4040%s\n", colorBold, colorReset)
	}
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  %sPress Ctrl+C to stop%s\n", colorDim, colorReset)
	fmt.Fprintln(colorableOut)
}

// PrintRequest logs a proxied request with color-coded status.
func (d *Display) PrintRequest(method, path string, status int, elapsed time.Duration) {
	plainLine := plainRequest(method, path, status, elapsed)
	isErr := status >= 500
	if d.sink != nil {
		d.sink.OnLog(plainLine, isErr)
	}
	if d.Quiet {
		return
	}
	color := statusColor(status)
	fmt.Fprintf(colorableOut, "  %s%-7s%s %s %s%d%s %s%s%s\n",
		colorBold, method, colorReset,
		path,
		color, status, colorReset,
		colorDim, elapsed.Round(time.Millisecond), colorReset)
}

// PrintDisconnected logs a disconnection event.
func (d *Display) PrintDisconnected(err error) {
	if d.sink != nil {
		d.sink.OnLog(plainDisconnected(err), true)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintf(colorableOut, "\n  %s✗ Disconnected: %v%s\n", colorRed, err, colorReset)
}

// PrintReconnecting logs a reconnection attempt.
func (d *Display) PrintReconnecting(backoff time.Duration) {
	if d.sink != nil {
		d.sink.OnLog(plainReconnecting(backoff), false)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintf(colorableOut, "  %s↻ Reconnecting in %s...%s\n", colorYellow, backoff, colorReset)
}

// PrintReconnected logs a successful reconnection with the same tunnel ID.
func (d *Display) PrintReconnected(publicURL string) {
	if d.sink != nil {
		d.sink.OnTunnelURL(publicURL)
		d.sink.OnLog(plainReconnected(publicURL), false)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintf(colorableOut, "\n  %s✓ Reconnected: %s%s\n\n", colorGreen, publicURL, colorReset)
}

// PrintTCPBanner prints the TCP tunnel connection banner.
func (d *Display) PrintTCPBanner(tcpAddr string, localPort int) {
	plainLine := plainTCPBanner(tcpAddr, localPort)
	if d.sink != nil {
		d.sink.OnTunnelURL(tcpAddr)
		d.sink.OnLog(plainLine, false)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "%s%stunnel%s %sTCP mode%s\n", colorBold, colorCyan, colorReset, colorGreen, colorReset)
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  Forwarding: %s%s%s → %slocalhost:%d%s\n",
		colorBold, tcpAddr, colorReset,
		colorDim, localPort, colorReset)
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  %sPress Ctrl+C to stop%s\n", colorDim, colorReset)
	fmt.Fprintln(colorableOut)
}

// PrintUDPBanner prints the UDP tunnel connection banner.
func (d *Display) PrintUDPBanner(udpAddr string, localPort int) {
	plainLine := plainUDPBanner(udpAddr, localPort)
	if d.sink != nil {
		d.sink.OnTunnelURL(udpAddr)
		d.sink.OnLog(plainLine, false)
	}
	if d.Quiet {
		return
	}
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "%s%stunnel%s %sUDP mode%s\n", colorBold, colorCyan, colorReset, colorGreen, colorReset)
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  Forwarding: %s%s%s → %slocalhost:%d%s\n",
		colorBold, udpAddr, colorReset,
		colorDim, localPort, colorReset)
	fmt.Fprintln(colorableOut)
	fmt.Fprintf(colorableOut, "  %sPress Ctrl+C to stop%s\n", colorDim, colorReset)
	fmt.Fprintln(colorableOut)
}

// PrintTCPStream logs a TCP stream open/close event.
func (d *Display) PrintTCPStream(streamID, remoteAddr string, opened bool) {
	if d.sink != nil {
		d.sink.OnLog(plainTCPStream(streamID, remoteAddr, opened), false)
	}
	if d.Quiet {
		return
	}
	if opened {
		fmt.Printf("  %s→%s %s%s%s stream %s\n",
			colorGreen, colorReset,
			colorBold, remoteAddr, colorReset,
			streamID[:8])
	} else {
		fmt.Printf("  %s←%s %s%s%s stream %s closed\n",
			colorRed, colorReset,
			colorDim, remoteAddr, colorReset,
			streamID[:8])
	}
}

func statusColor(code int) string {
	switch {
	case code >= 200 && code < 300:
		return colorGreen
	case code >= 300 && code < 400:
		return colorYellow
	default:
		return colorRed
	}
}
