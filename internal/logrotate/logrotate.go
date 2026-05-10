//go:build daemon

// Package logrotate provides a size-based rotating file writer.
package logrotate

import (
	"fmt"
	"os"
	"sync"
)

// Writer is a size-rotated log file. maxSize == 0 disables rotation.
//
// Concurrency: every public method takes w.mu. The private rotate() must only
// be called by methods that already hold w.mu (currently Write only) and must
// fully reassign w.file before returning so subsequent writes hit a valid fd.
type Writer struct {
	path     string
	maxSize  int64
	maxFiles int

	mu      sync.Mutex
	file    *os.File
	written int64
	closed  bool
}

// New opens path for append and returns a rotating Writer.
func New(path string, maxSize int64, maxFiles int) (*Writer, error) {
	if maxFiles <= 0 {
		maxFiles = 3
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{path: path, maxSize: maxSize, maxFiles: maxFiles, file: f, written: info.Size()}, nil
}

// Write appends p to the log, rotating if maxSize would be exceeded.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return 0, os.ErrClosed
	}

	n, err := w.file.Write(p)
	w.written += int64(n)
	if err == nil && w.maxSize > 0 && w.written >= w.maxSize {
		if rerr := w.rotateLocked(); rerr != nil {
			// Rotation failed; drop the rotated handle to avoid double-close,
			// but keep the writer "closed" so subsequent Write returns ErrClosed
			// instead of silently writing to a stale fd.
			w.closed = true
			return n, rerr
		}
	}
	return n, err
}

// Close closes the current file. Safe to call multiple times.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

// rotateLocked renames the current file and opens a new one.
// PRECONDITION: w.mu is held by the caller.
func (w *Writer) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("rotate close: %w", err)
	}
	w.file = nil

	for i := w.maxFiles; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		if i == w.maxFiles {
			os.Remove(old)
		} else {
			os.Rename(old, fmt.Sprintf("%s.%d", w.path, i+1))
		}
	}
	os.Rename(w.path, w.path+".1")

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		// Surface the error rather than blindly reopening in append mode and
		// hiding the failure — caller (Write) will mark the writer closed.
		return fmt.Errorf("rotate open: %w", err)
	}
	w.file = f
	w.written = 0
	return nil
}
