package server

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// tunnelIDLength is the random-ID length in lowercase base36 chars. 16 chars
	// provides ~83 bits of entropy while staying safe for DNS subdomain routing,
	// where hostnames are case-insensitive and commonly lowercased by clients.
	tunnelIDLength            = 16
	dnsIDChars                = "abcdefghijklmnopqrstuvwxyz0123456789"
	reservationTimeout        = 60 * time.Second
	defaultMaxTunnelsPerToken = 50
	defaultMaxTunnelsPerIP    = 100
)

type reservation struct {
	token   string
	expires time.Time
}

// Hub manages all active tunnels, post-disconnect reservations, and in-flight
// name claims. claims tracks names that a connecting client has reserved but
// not yet Register()ed; this prevents two simultaneous --name connects from
// both succeeding for the same name.
type Hub struct {
	mu           sync.RWMutex
	tunnels      map[string]*Tunnel
	reservations map[string]reservation
	claims       map[string]string // name -> claiming token

	maxPerToken int
	maxPerIP    int
	tokenCount  map[string]int // active+reserved-slot count per token
	ipCount     map[string]int // active+reserved-slot count per remote IP
}

// NewHub creates a new Hub. Per-token and per-IP caps are read from
// TUNNEL_MAX_TUNNELS_PER_TOKEN and TUNNEL_MAX_TUNNELS_PER_IP. A value of 0
// disables the corresponding cap.
func NewHub() *Hub {
	return &Hub{
		tunnels:      make(map[string]*Tunnel),
		reservations: make(map[string]reservation),
		claims:       make(map[string]string),
		maxPerToken:  envInt("TUNNEL_MAX_TUNNELS_PER_TOKEN", defaultMaxTunnelsPerToken),
		maxPerIP:     envInt("TUNNEL_MAX_TUNNELS_PER_IP", defaultMaxTunnelsPerIP),
		tokenCount:   make(map[string]int),
		ipCount:      make(map[string]int),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// AcquireSlot reserves capacity for an upcoming tunnel registration. Returns
// nil if the slot is allocated; callers MUST invoke the returned release
// function exactly once after Unregister or on any pre-Register failure.
// Returns an error string when caps are exceeded.
func (h *Hub) AcquireSlot(token, ip string) (release func(), err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxPerToken > 0 && token != "" && h.tokenCount[token] >= h.maxPerToken {
		return nil, fmt.Sprintf("per-token tunnel cap reached (%d)", h.maxPerToken)
	}
	if h.maxPerIP > 0 && ip != "" && h.ipCount[ip] >= h.maxPerIP {
		return nil, fmt.Sprintf("per-IP tunnel cap reached (%d)", h.maxPerIP)
	}
	if token != "" {
		h.tokenCount[token]++
	}
	if ip != "" {
		h.ipCount[ip]++
	}
	released := false
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if released {
			return
		}
		released = true
		if token != "" {
			if n := h.tokenCount[token] - 1; n <= 0 {
				delete(h.tokenCount, token)
			} else {
				h.tokenCount[token] = n
			}
		}
		if ip != "" {
			if n := h.ipCount[ip] - 1; n <= 0 {
				delete(h.ipCount, ip)
			} else {
				h.ipCount[ip] = n
			}
		}
	}, ""
}

// Register adds a tunnel to the hub and clears any reservation or in-flight
// claim for that ID.
func (h *Hub) Register(t *Tunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tunnels[t.ID] = t
	delete(h.reservations, t.ID)
	delete(h.claims, t.ID)
}

// Unregister removes a tunnel from the hub and reserves the ID for the given token.
func (h *Hub) Unregister(id string, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tunnels, id)
	if token != "" {
		h.reservations[id] = reservation{
			token:   token,
			expires: time.Now().Add(reservationTimeout),
		}
	}
}

// ClaimReservation checks if a tunnel ID is reserved for the given token.
// Returns true and clears the reservation if valid. Returns false if the ID
// is actively in use, not reserved, expired, or belongs to a different token.
func (h *Hub) ClaimReservation(id string, token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Can't claim if another tunnel is actively using this ID.
	if _, active := h.tunnels[id]; active {
		return false
	}

	res, ok := h.reservations[id]
	if !ok {
		return false
	}
	if time.Now().After(res.expires) {
		delete(h.reservations, id)
		return false
	}
	if res.token != token {
		return false
	}
	delete(h.reservations, id)
	return true
}

// Get returns a tunnel by ID, or nil if not found.
func (h *Hub) Get(id string) *Tunnel {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tunnels[id]
}

// GetFold returns a tunnel using case-insensitive ID matching. It exists for
// base-domain routing because DNS hostnames are case-insensitive: a random ID
// generated before IDs became lowercase-only can appear lowercased in Host.
func (h *Hub) GetFold(id string) *Tunnel {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if t := h.tunnels[id]; t != nil {
		return t
	}
	for tunnelID, t := range h.tunnels {
		if strings.EqualFold(tunnelID, id) {
			return t
		}
	}
	return nil
}

// List returns all active tunnels.
func (h *Hub) List() []*Tunnel {
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := make([]*Tunnel, 0, len(h.tunnels))
	for _, t := range h.tunnels {
		list = append(list, t)
	}
	return list
}

// Count returns the number of currently active tunnels.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.tunnels)
}

// GenerateID creates a unique 16-character lowercase DNS-safe tunnel ID.
// Returns an error if crypto/rand fails or no unique ID is found after 1000 attempts.
func (h *Hub) GenerateID() (string, error) {
	const maxAttempts = 1000
	for i := 0; i < maxAttempts; i++ {
		id, err := randomDNSID(tunnelIDLength)
		if err != nil {
			return "", err
		}
		h.mu.Lock()
		_, inUse := h.tunnels[id]
		_, reserved := h.reservations[id]
		_, claimed := h.claims[id]
		if !inUse && !reserved && !claimed {
			h.mu.Unlock()
			return id, nil
		}
		h.mu.Unlock()
	}
	return "", fmt.Errorf("failed to generate unique tunnel ID after %d attempts", maxAttempts)
}

// ClaimName atomically reserves a name for an upcoming Register. Returns true
// if the name is currently free (no active tunnel, no in-flight claim, and
// either no reservation or the reservation belongs to the same token, or has
// expired).
//
// On success the caller MUST eventually call either Register (which clears the
// claim) or ReleaseName (on connection setup failure). On false return, the
// caller should respond to the client with an error and not proceed.
func (h *Hub) ClaimName(name, token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, active := h.tunnels[name]; active {
		return false
	}
	if _, pending := h.claims[name]; pending {
		return false
	}
	if res, ok := h.reservations[name]; ok {
		if !time.Now().After(res.expires) && res.token != token {
			return false
		}
		// Consume the reservation (whether expired or same-token) so a concurrent
		// ClaimReservation for this name cannot also succeed.
		delete(h.reservations, name)
	}
	h.claims[name] = token
	return true
}

// ReleaseName clears an in-flight claim made by ClaimName. The token must
// match the original claimant; mismatches are silently ignored.
func (h *Hub) ReleaseName(name, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.claims[name]; ok && t == token {
		delete(h.claims, name)
	}
}

func randomDNSID(n int) (string, error) {
	b := make([]byte, n)
	maxChar := big.NewInt(int64(len(dnsIDChars)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, maxChar)
		if err != nil {
			return "", fmt.Errorf("crypto/rand: %w", err)
		}
		b[i] = dnsIDChars[idx.Int64()]
	}
	return string(b), nil
}
