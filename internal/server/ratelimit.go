package server

import (
	"sync"
	"time"
)

const (
	RateLimitWindow          = time.Minute
	RateLimitMax             = 10 // max failed auth attempts per IP per window
	RateLimitCleanupInterval = 5 * time.Minute

	// Test constants with shorter durations for unit testing
	testRateLimitWindow          = 100 * time.Millisecond
	testRateLimitCleanupInterval = 50 * time.Millisecond
)

// RateLimiter tracks failed authentication attempts per IP address.
// Note: This in-memory implementation does not share state across multiple
// server instances. Horizontal deployments need a distributed store such as Redis.
type RateLimiter struct {
	mu        sync.Mutex
	entries   map[string]*rateLimitEntry
	window    time.Duration
	done      chan struct{}
	closeOnce sync.Once
}

type rateLimitEntry struct {
	attempts []time.Time
}

// NewRateLimiter creates a RateLimiter and starts a background cleanup goroutine.
func NewRateLimiter() *RateLimiter {
	return newRateLimiter(RateLimitWindow, RateLimitCleanupInterval)
}

// NewRateLimiterForTest creates a RateLimiter with custom timing for tests.
func NewRateLimiterForTest(window, cleanupInterval time.Duration) *RateLimiter {
	return newRateLimiter(window, cleanupInterval)
}

func newRateLimiter(window, cleanupInterval time.Duration) *RateLimiter {
	rl := &RateLimiter{
		entries: make(map[string]*rateLimitEntry),
		window:  window,
		done:    make(chan struct{}),
	}
	go rl.cleanupWithInterval(cleanupInterval)
	return rl
}

// RecordFailure records a failed authentication attempt for the given IP.
func (rl *RateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.entries[ip]
	if !ok {
		e = &rateLimitEntry{}
		rl.entries[ip] = e
	}
	e.attempts = append(e.attempts, time.Now())
}

// IsLimited returns true if the IP has exceeded the rate limit.
func (rl *RateLimiter) IsLimited(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e, ok := rl.entries[ip]
	if !ok {
		return false
	}
	cutoff := time.Now().Add(-rl.window)
	valid := e.attempts[:0]
	for _, t := range e.attempts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	e.attempts = valid
	return len(valid) >= RateLimitMax
}

func (rl *RateLimiter) cleanupWithInterval(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.done:
			return
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for ip, e := range rl.entries {
				valid := e.attempts[:0]
				for _, t := range e.attempts {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(rl.entries, ip)
				} else {
					e.attempts = valid
				}
			}
			rl.mu.Unlock()
		}
	}
}

// Close stops the background cleanup goroutine. It is safe to call multiple times.
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		close(rl.done)
	})
}
