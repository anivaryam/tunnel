package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func mockTunnel(id string) *Tunnel {
	return &Tunnel{ID: id}
}

func TestHubRegisterUnregister(t *testing.T) {
	h := NewHub()

	h.Register(mockTunnel("testid1"))

	list := h.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 tunnel after register, got %d", len(list))
	}
	if list[0].ID != "testid1" {
		t.Fatalf("expected tunnel ID 'testid1', got '%s'", list[0].ID)
	}

	h.Unregister("testid1", "token123")

	list = h.List()
	if len(list) != 0 {
		t.Fatalf("expected 0 tunnels after unregister, got %d", len(list))
	}

	if h.Get("testid1") != nil {
		t.Fatal("expected nil after unregister")
	}
}

func TestHubCount(t *testing.T) {
	h := NewHub()

	if got := h.Count(); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}

	h.Register(mockTunnel("a"))
	if got := h.Count(); got != 1 {
		t.Fatalf("expected 1 after register, got %d", got)
	}

	h.Register(mockTunnel("b"))
	if got := h.Count(); got != 2 {
		t.Fatalf("expected 2 after second register, got %d", got)
	}

	h.Unregister("a", "")
	if got := h.Count(); got != 1 {
		t.Fatalf("expected 1 after unregister, got %d", got)
	}
}

func TestHubGenerateIDUniqueness(t *testing.T) {
	h := NewHub()
	seen := make(map[string]bool)

	for i := 0; i < 1000; i++ {
		id, err := h.GenerateID()
		if err != nil {
			t.Fatalf("GenerateID failed: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestHubGenerateIDUsesDNSLowercaseChars(t *testing.T) {
	h := NewHub()
	id, err := h.GenerateID()
	if err != nil {
		t.Fatalf("GenerateID failed: %v", err)
	}
	if id != strings.ToLower(id) {
		t.Fatalf("GenerateID returned mixed-case DNS label %q", id)
	}
}

func TestHubGenerateIDNoCollisionWithExisting(t *testing.T) {
	h := NewHub()

	for i := 0; i < 10; i++ {
		h.Register(mockTunnel("tunnel" + string(rune('0'+i))))
	}

	h.mu.Lock()
	for i := 0; i < 10; i++ {
		h.reservations["reserved"+string(rune('0'+i))] = reservation{
			token:   "token",
			expires: time.Now().Add(10 * time.Second),
		}
	}
	h.mu.Unlock()

	seen := make(map[string]bool)
	for _, id := range []string{"tunnel0", "tunnel1", "reserved0", "reserved1"} {
		seen[id] = true
	}

	for i := 0; i < 100; i++ {
		id, err := h.GenerateID()
		if err != nil {
			t.Fatalf("GenerateID failed: %v", err)
		}
		if seen[id] {
			t.Fatalf("generated ID %s collided with existing tunnel/reservation", id)
		}
		seen[id] = true
	}
}

func TestHubClaimReservation(t *testing.T) {
	h := NewHub()

	h.mu.Lock()
	h.reservations["valid"] = reservation{
		token:   "correct-token",
		expires: time.Now().Add(10 * time.Second),
	}
	h.mu.Unlock()

	ok := h.ClaimReservation("valid", "correct-token")
	if !ok {
		t.Fatal("expected valid reservation claim to succeed")
	}

	h.mu.Lock()
	_, exists := h.reservations["valid"]
	h.mu.Unlock()
	if exists {
		t.Fatal("reservation should have been cleared after successful claim")
	}

	h.mu.Lock()
	h.reservations["expired"] = reservation{
		token:   "token",
		expires: time.Now().Add(-1 * time.Second),
	}
	h.mu.Unlock()

	ok = h.ClaimReservation("expired", "token")
	if ok {
		t.Fatal("expected expired reservation claim to fail")
	}

	h.mu.Lock()
	_, exists = h.reservations["expired"]
	h.mu.Unlock()
	if exists {
		t.Fatal("expired reservation should have been deleted")
	}

	h.mu.Lock()
	h.reservations["wrongtoken"] = reservation{
		token:   "correct-token",
		expires: time.Now().Add(10 * time.Second),
	}
	h.mu.Unlock()

	ok = h.ClaimReservation("wrongtoken", "wrong-token")
	if ok {
		t.Fatal("expected wrong token claim to fail")
	}

	h.mu.Lock()
	_, exists = h.reservations["wrongtoken"]
	h.mu.Unlock()
	if !exists {
		t.Fatal("reservation should still exist after failed claim due to wrong token")
	}

	h.Register(mockTunnel("active"))
	h.mu.Lock()
	h.reservations["active"] = reservation{
		token:   "token",
		expires: time.Now().Add(10 * time.Second),
	}
	h.mu.Unlock()

	ok = h.ClaimReservation("active", "token")
	if ok {
		t.Fatal("expected claim to fail when ID is in active use")
	}

	ok = h.ClaimReservation("nonexistent", "token")
	if ok {
		t.Fatal("expected claim to fail for non-existent reservation")
	}
}

func TestHubClaimReservationRace(t *testing.T) {
	h := NewHub()

	h.mu.Lock()
	h.reservations["race-id"] = reservation{
		token:   "race-token",
		expires: time.Now().Add(10 * time.Second),
	}
	h.mu.Unlock()

	const numGoroutines = 10
	var wg sync.WaitGroup
	successCount := 0
	var countMu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.ClaimReservation("race-id", "race-token") {
				countMu.Lock()
				successCount++
				countMu.Unlock()
			}
		}()
	}

	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful claim, got %d", successCount)
	}
}

func TestHubGet(t *testing.T) {
	h := NewHub()

	if h.Get("nonexistent") != nil {
		t.Fatal("expected nil for non-existent tunnel")
	}

	h.Register(mockTunnel("gettest"))

	retrieved := h.Get("gettest")
	if retrieved == nil {
		t.Fatal("expected to get registered tunnel")
	}
	if retrieved.ID != "gettest" {
		t.Fatalf("expected ID 'gettest', got '%s'", retrieved.ID)
	}
}

func TestHubGetFoldFindsMixedCaseTunnelID(t *testing.T) {
	h := NewHub()
	tunnel := mockTunnel("AbC123xYz")
	h.Register(tunnel)

	if got := h.GetFold("abc123xyz"); got != tunnel {
		t.Fatal("expected case-insensitive lookup to find mixed-case tunnel ID")
	}
}

func TestHubList(t *testing.T) {
	h := NewHub()

	list := h.List()
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d tunnels", len(list))
	}

	tunnels := []*Tunnel{
		mockTunnel("list1"),
		mockTunnel("list2"),
		mockTunnel("list3"),
	}
	for _, tt := range tunnels {
		h.Register(tt)
	}

	list = h.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 tunnels, got %d", len(list))
	}

	ids := make(map[string]bool)
	for _, tt := range list {
		ids[tt.ID] = true
	}
	for _, id := range []string{"list1", "list2", "list3"} {
		if !ids[id] {
			t.Fatalf("expected ID '%s' in list", id)
		}
	}
}

func TestHubGenerateIDRace(t *testing.T) {
	h := NewHub()

	const numGoroutines = 20
	const idsPerGoroutine = 50
	var wg sync.WaitGroup
	seen := make(map[string]bool)
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < idsPerGoroutine; j++ {
				id, err := h.GenerateID()
				if err != nil {
					t.Errorf("GenerateID failed: %v", err)
					continue
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate ID generated: %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
}

func TestHubRegisterClearsReservation(t *testing.T) {
	h := NewHub()

	h.mu.Lock()
	h.reservations["tobecleared"] = reservation{
		token:   "token",
		expires: time.Now().Add(10 * time.Second),
	}
	h.mu.Unlock()

	h.mu.Lock()
	_, exists := h.reservations["tobecleared"]
	h.mu.Unlock()
	if !exists {
		t.Fatal("expected reservation to exist before register")
	}

	h.Register(mockTunnel("tobecleared"))

	h.mu.Lock()
	_, exists = h.reservations["tobecleared"]
	h.mu.Unlock()
	if exists {
		t.Fatal("expected reservation to be cleared after register")
	}
}

func TestHubClaimNameSuccess(t *testing.T) {
	h := NewHub()
	if !h.ClaimName("eservice", "tok1") {
		t.Fatal("expected first claim of free name to succeed")
	}
	if h.ClaimName("eservice", "tok2") {
		t.Fatal("expected second claim of same name to fail")
	}
	if h.ClaimName("eservice", "tok1") {
		t.Fatal("expected re-claim by same token while still pending to fail")
	}
}

func TestHubClaimNameRejectsActive(t *testing.T) {
	h := NewHub()
	h.Register(mockTunnel("eservice"))
	if h.ClaimName("eservice", "tok1") {
		t.Fatal("expected claim of active tunnel name to fail")
	}
}

func TestHubClaimNameAllowsReservedSameToken(t *testing.T) {
	h := NewHub()
	h.Unregister("eservice", "tok1") // creates a reservation for tok1
	if !h.ClaimName("eservice", "tok1") {
		t.Fatal("expected same-token claim of reserved name to succeed")
	}
	h.mu.Lock()
	_, stillReserved := h.reservations["eservice"]
	h.mu.Unlock()
	if stillReserved {
		t.Fatal("expected same-token reservation to be consumed after ClaimName")
	}
}

func TestHubClaimNameRejectsReservedOtherToken(t *testing.T) {
	h := NewHub()
	h.Unregister("eservice", "tok1")
	if h.ClaimName("eservice", "tok2") {
		t.Fatal("expected other-token claim of reserved name to fail")
	}
}

func TestHubClaimNameAllowsExpiredReservation(t *testing.T) {
	h := NewHub()
	h.mu.Lock()
	h.reservations["eservice"] = reservation{
		token:   "tok1",
		expires: time.Now().Add(-time.Second),
	}
	h.mu.Unlock()
	if !h.ClaimName("eservice", "tok2") {
		t.Fatal("expected claim past expired reservation to succeed for any token")
	}
	h.mu.Lock()
	_, stillReserved := h.reservations["eservice"]
	h.mu.Unlock()
	if stillReserved {
		t.Fatal("expected expired reservation to be deleted after ClaimName")
	}
}

func TestHubReleaseName(t *testing.T) {
	h := NewHub()
	h.ClaimName("eservice", "tok1")
	h.ReleaseName("eservice", "tok1")
	if !h.ClaimName("eservice", "tok2") {
		t.Fatal("expected name to be claimable after release")
	}
}

func TestHubReleaseNameWrongTokenIsNoop(t *testing.T) {
	h := NewHub()
	h.ClaimName("eservice", "tok1")
	h.ReleaseName("eservice", "tok2")
	if h.ClaimName("eservice", "tok2") {
		t.Fatal("ReleaseName from wrong token should not free the claim")
	}
}

func TestHubRegisterClearsClaim(t *testing.T) {
	h := NewHub()
	if !h.ClaimName("eservice", "tok1") {
		t.Fatal("setup: claim failed")
	}
	h.Register(mockTunnel("eservice"))
	// After register, the claim entry should be gone (Register cleared it).
	// Re-claiming requires the active tunnel to be unregistered first.
	h.Unregister("eservice", "tok1")
	if !h.ClaimName("eservice", "tok1") {
		t.Fatal("expected claim to succeed after unregister; Register may have leaked claim state")
	}
}

func TestHubGenerateIDSkipsClaimed(t *testing.T) {
	h := NewHub()
	h.ClaimName("specific-claim", "tok1")
	for i := 0; i < 100; i++ {
		id, err := h.GenerateID()
		if err != nil {
			t.Fatalf("GenerateID: %v", err)
		}
		if id == "specific-claim" {
			t.Fatalf("GenerateID returned a claimed ID")
		}
	}
}

func TestHubClaimNameRace(t *testing.T) {
	h := NewHub()
	const goroutines = 20
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.ClaimName("contested", "tok1") {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful claim, got %d", successCount)
	}
}

func TestHubAcquireSlotCaps(t *testing.T) {
	h := NewHub()
	h.maxPerToken = 2
	h.maxPerIP = 3

	r1, errStr := h.AcquireSlot("tok", "1.1.1.1")
	if errStr != "" || r1 == nil {
		t.Fatalf("first slot must succeed, got %q", errStr)
	}
	r2, errStr := h.AcquireSlot("tok", "1.1.1.1")
	if errStr != "" || r2 == nil {
		t.Fatalf("second slot must succeed, got %q", errStr)
	}
	if _, errStr = h.AcquireSlot("tok", "1.1.1.1"); errStr == "" {
		t.Fatal("third slot must fail (per-token cap=2)")
	}

	if _, errStr = h.AcquireSlot("tok2", "1.1.1.1"); errStr != "" {
		t.Fatalf("third slot for new token from same IP must succeed up to per-ip cap, got %q", errStr)
	}
	if _, errStr = h.AcquireSlot("tok3", "1.1.1.1"); errStr == "" {
		t.Fatal("fourth slot from same IP must fail (per-ip cap=3)")
	}

	r1()
	if _, errStr = h.AcquireSlot("tok", "1.1.1.1"); errStr != "" {
		t.Fatalf("slot must succeed after release, got %q", errStr)
	}
}

func TestHubAcquireSlotZeroCapsDisabled(t *testing.T) {
	h := NewHub()
	h.maxPerToken = 0
	h.maxPerIP = 0
	for i := 0; i < 200; i++ {
		if _, errStr := h.AcquireSlot("tok", "1.1.1.1"); errStr != "" {
			t.Fatalf("cap=0 must allow unlimited slots, got %q at %d", errStr, i)
		}
	}
}

func TestHubClaimNameRaceDifferentTokens(t *testing.T) {
	h := NewHub()
	const goroutines = 20
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		token := fmt.Sprintf("tok%d", i)
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			if h.ClaimName("contested", tok) {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(token)
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful claim across different tokens, got %d", successCount)
	}
}
