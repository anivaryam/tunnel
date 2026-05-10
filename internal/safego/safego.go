// Package safego runs goroutines under a recover() so a single panic does
// not crash the whole process. Use Go(label, fn) instead of `go fn()` for
// any long-lived pump or per-connection handler — the label lands in the
// log line emitted on recovery, plus the full stack via runtime/debug.
package safego

import (
	"log"
	"runtime/debug"
)

// Go runs fn in a new goroutine. If fn panics, the panic is recovered and
// logged with the supplied label and a full stack trace.
func Go(label string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[panic %s] %v\n%s", label, r, debug.Stack())
			}
		}()
		fn()
	}()
}
