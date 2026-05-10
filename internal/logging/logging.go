// Package logging owns the global slog handler used by the tunnel binaries.
// Default level INFO; SetDebug(true) flips to DEBUG. SetText switches to a
// human-friendly text handler (default) vs SetJSON for structured logs.
package logging

import (
	"io"
	"log"
	"log/slog"
	"os"
	"sync"
)

var (
	mu       sync.Mutex
	level    = new(slog.LevelVar)
	useJSON  = false
	output   io.Writer
	handlerR slog.Handler
)

func init() {
	level.Set(slog.LevelInfo)
	output = os.Stderr
	rebuild()
}

func rebuild() {
	if useJSON {
		handlerR = slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
		slog.SetDefault(slog.New(handlerR))
		log.SetFlags(0)
		log.SetOutput(slogWriter{})
		return
	}
	// Default text mode: leave stdlib log alone (plain prefix-less output)
	// and only route slog calls (Debug/Warn/Error) through a compact handler
	// for emerging structured logging. INFO via slog is suppressed since
	// existing log.Printf already covers human-readable output.
	handlerR = slog.NewTextHandler(output, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	slog.SetDefault(slog.New(handlerR))
	log.SetFlags(0)
	log.SetOutput(output)
}

// SetDebug toggles DEBUG logging.
func SetDebug(on bool) {
	mu.Lock()
	defer mu.Unlock()
	if on {
		level.Set(slog.LevelDebug)
	} else {
		level.Set(slog.LevelInfo)
	}
}

// SetJSON switches between text and JSON output.
func SetJSON(on bool) {
	mu.Lock()
	defer mu.Unlock()
	useJSON = on
	rebuild()
}

// SetOutput redirects the global handler. Used by daemon mode to send logs
// to the rotating log file.
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	output = w
	rebuild()
}

// Debug logs at DEBUG level. Skipped when the level is INFO+.
func Debug(msg string, args ...any) { slog.Debug(msg, args...) }

// slogWriter forwards writes from stdlib log.Printf into slog at INFO.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	msg := string(p)
	for len(msg) > 0 && (msg[len(msg)-1] == '\n' || msg[len(msg)-1] == '\r') {
		msg = msg[:len(msg)-1]
	}
	if msg != "" {
		slog.Info(msg)
	}
	return len(p), nil
}
