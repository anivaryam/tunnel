package server

import (
	"testing"
	"time"
)

func TestRateLimiterNotLimitedInitially(t *testing.T) {
	rl := NewRateLimiter()
	if rl.IsLimited("192.168.1.1") {
		t.Error("expected new IP to not be limited")
	}
}

func TestRateLimiterRecordsFailures(t *testing.T) {
	rl := NewRateLimiter()
	ip := "192.168.1.1"

	if rl.IsLimited(ip) {
		t.Error("expected IP to not be limited initially")
	}

	rl.RecordFailure(ip)
	if rl.IsLimited(ip) {
		t.Error("expected IP to not be limited after 1 failure")
	}

	rl.RecordFailure(ip)
	if rl.IsLimited(ip) {
		t.Error("expected IP to not be limited after 2 failures")
	}

	for i := 2; i < RateLimitMax; i++ {
		rl.RecordFailure(ip)
	}

	if !rl.IsLimited(ip) {
		t.Error("expected IP to be limited after max failures")
	}
}

func TestRateLimiterIsLimited(t *testing.T) {
	rl := NewRateLimiter()
	ip := "192.168.1.1"

	for i := 0; i < RateLimitMax; i++ {
		rl.RecordFailure(ip)
	}

	if !rl.IsLimited(ip) {
		t.Error("expected IP to be limited after max failures")
	}
}

func TestRateLimiterExpires(t *testing.T) {
	rl := NewRateLimiterForTest(testRateLimitWindow, testRateLimitCleanupInterval)
	ip := "192.168.1.1"

	for i := 0; i < RateLimitMax; i++ {
		rl.RecordFailure(ip)
	}

	if !rl.IsLimited(ip) {
		t.Error("expected IP to be limited immediately after failures")
	}

	time.Sleep(testRateLimitWindow + 10*time.Millisecond)

	if rl.IsLimited(ip) {
		t.Error("expected IP to no longer be limited after window expiration")
	}
}

func TestRateLimiterMultipleIPs(t *testing.T) {
	rl := NewRateLimiter()
	ip1 := "192.168.1.1"
	ip2 := "192.168.1.2"

	for i := 0; i < RateLimitMax; i++ {
		rl.RecordFailure(ip1)
	}

	if !rl.IsLimited(ip1) {
		t.Error("expected ip1 to be limited")
	}

	if rl.IsLimited(ip2) {
		t.Error("expected ip2 to not be limited")
	}
}

func TestRateLimiterCleanupPrunes(t *testing.T) {
	rl := NewRateLimiterForTest(testRateLimitWindow, testRateLimitCleanupInterval)
	ip := "192.168.1.1"

	for i := 0; i < RateLimitMax; i++ {
		rl.RecordFailure(ip)
	}

	time.Sleep(testRateLimitCleanupInterval + 50*time.Millisecond)

	if rl.IsLimited(ip) {
		t.Error("expected cleanup to remove expired entries")
	}
}

func TestRateLimiterCloseStopsCleanupLoop(t *testing.T) {
	rl := NewRateLimiterForTest(testRateLimitWindow, testRateLimitCleanupInterval)
	rl.Close()
	rl.Close()
	time.Sleep(10 * time.Millisecond)
	rl.RecordFailure("192.168.1.1")
	if rl.IsLimited("192.168.1.1") {
		t.Error("expected IP to not be limited after close")
	}
}
