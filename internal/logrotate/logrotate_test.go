//go:build daemon

package logrotate

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotateOnSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.log")

	w, err := New(p, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("0123456789ABC")); err != nil { // > 10 bytes
		t.Fatal(err)
	}

	// Original must be moved to .1, fresh file present.
	rotated, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if !bytes.Equal(rotated, []byte("0123456789ABC")) {
		t.Fatalf("rotated content = %q", rotated)
	}
	cur, _ := os.Stat(p)
	if cur == nil || cur.Size() != 0 {
		t.Fatalf("current file should be empty after rotate")
	}
}

func TestZeroMaxSizeNoRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.log")

	w, err := New(p, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 100; i++ {
		w.Write([]byte("hello\n"))
	}
	if _, err := os.Stat(p + ".1"); !os.IsNotExist(err) {
		t.Fatalf("should not have rotated: %v", err)
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.log")

	w, err := New(p, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("double close should be a no-op, got %v", err)
	}
	if _, err := w.Write([]byte("x")); err != os.ErrClosed {
		t.Fatalf("write after close: got %v, want os.ErrClosed", err)
	}
}

func TestRotateUnderConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "concurrent.log")

	w, err := New(p, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := w.Write([]byte("0123456789ABCDEF\n")); err != nil && err != os.ErrClosed {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("expected rotated file: %v", err)
	}
}
