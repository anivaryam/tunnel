package safego

import (
	"sync"
	"testing"
	"time"
)

func TestGoRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	Go("test.panic", func() {
		defer wg.Done()
		panic("boom")
	})
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Go() did not recover panic")
	}
}

func TestGoRunsNormally(t *testing.T) {
	ran := make(chan struct{})
	Go("test.normal", func() {
		close(ran)
	})
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("function did not execute")
	}
}
