package server

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/coder/websocket"
)

// Config holds server configuration.
type Config struct {
	Port             string
	PublicHost       string   // optional public hostname for TCP/UDP addresses; derived from request Host if empty
	BaseDomain       string   // optional base domain for subdomain routing (e.g. "relay.example.com"); tunnels become {id}.relay.example.com
	WorkerSecret     string   // optional shared secret validated on X-Worker-Secret header from Cloudflare Worker
	CFWorkerEnabled  bool     // if true, require WorkerSecret for Cloudflare Worker routing; if false (default), use direct DNS subdomain routing when BASE_DOMAIN is set
	SingleTunnelMode bool     // if true, only one tunnel may connect; root / proxies directly to it
	WSOriginPatterns []string // optional Origin allowlist for /ws/connect (CLI control channel); empty disables Origin checks (back-compat default)
	HeroLottieURL    string   // optional Lottie JSON URL rendered as hero asset on the public landing page; empty hides the hero panel
}

// Server holds the HTTP handler.
type Server struct {
	Handler http.Handler
	Hub     *Hub
	limiter *RateLimiter
}

// New creates and returns a configured Server.
func New(cfg Config) *Server {
	hub := NewHub()
	auth := NewAuth()
	metrics := NewMetrics()
	limiter := NewRateLimiter()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	mux.Handle("GET /metrics", metricsAuthMiddleware(auth, metrics.Handler()))
	mux.HandleFunc("GET /dashboard", DashboardHandler(hub, auth))
	mux.HandleFunc("GET /dashboard/api", DashboardAPIHandler(hub, auth))
	mux.HandleFunc("GET /ws/connect", wsConnectHandler(hub, auth, cfg, metrics, limiter))
	mux.HandleFunc("/t/", ProxyHandler(hub, metrics))
	mux.HandleFunc("/", rootHandler(hub, metrics, cfg))

	var handler http.Handler = mux
	if cfg.BaseDomain != "" {
		handler = subdomainMiddleware(mux, hub, metrics, cfg)
	}

	return &Server{
		Handler: handler,
		Hub:     hub,
		limiter: limiter,
	}
}

// Close releases background resources owned by the Server.
func (s *Server) Close() {
	if s.limiter != nil {
		s.limiter.Close()
	}
}

// subdomainMiddleware intercepts requests where the Host is {tunnelID}.{baseDomain}
// and proxies them directly to the tunnel without the /t/ path prefix.
// Control paths (/ws/connect, /health, /metrics, /dashboard) are always handled
// by the server mux, even on tunnel subdomains.
func subdomainMiddleware(fallback http.Handler, hub *Hub, metrics *Metrics, cfg Config) http.Handler {
	controlPaths := map[string]bool{
		"/ws/connect":    true,
		"/health":        true,
		"/metrics":       true,
		"/dashboard":     true,
		"/dashboard/api": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tunnelID := ""
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		hostLower := strings.ToLower(host)
		baseDomain := strings.ToLower(cfg.BaseDomain)
		suffix := "." + baseDomain

		// 1) Cloudflare Worker path: secret AND host suffix must both match.
		// Without the host check, anyone holding a leaked WORKER_SECRET could
		// route arbitrary subdomains via this server.
		if cfg.CFWorkerEnabled && cfg.WorkerSecret != "" && baseDomain != "" {
			secret := r.Header.Get("X-Worker-Secret")
			if subtle.ConstantTimeCompare([]byte(secret), []byte(cfg.WorkerSecret)) == 1 &&
				(hostLower == baseDomain || strings.HasSuffix(hostLower, suffix)) {
				tunnelID = r.Header.Get("X-Forwarded-Tunnel")
			}
		}
		// 2) Direct DNS subdomain path (if BASE_DOMAIN set and CF path not matched).
		// Compare case-insensitively but preserve original case in the tunnel ID
		// since base62 IDs include uppercase characters.
		if tunnelID == "" && baseDomain != "" {
			if strings.HasSuffix(hostLower, suffix) {
				tunnelID = host[:len(host)-len(suffix)]
			}
		}
		if tunnelID == "" {
			fallback.ServeHTTP(w, r)
			return
		}

		// Never proxy server control paths through tunnels.
		if controlPaths[r.URL.Path] {
			fallback.ServeHTTP(w, r)
			return
		}

		tunnel := hub.GetFold(tunnelID)
		if tunnel == nil {
			http.Error(w, "tunnel not found", http.StatusBadGateway)
			return
		}

		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, tunnel, tunnelID, r.URL.Path, metrics)
			return
		}

		proxyRequest(w, r, tunnel, tunnelID, r.URL.Path, metrics, true)
	})
}

// metricsAuthMiddleware gates /metrics behind the same token as the rest of
// the server. Open mode (no TUNNEL_AUTH_TOKENS) leaves the endpoint open so
// existing scrape configs keep working; once any token is configured the
// scraper must present it via Authorization: Bearer or ?token=.
func metricsAuthMiddleware(auth *Auth, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		} else {
			token = r.URL.Query().Get("token")
		}
		if !auth.Validate(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func wsConnectHandler(hub *Hub, auth *Auth, cfg Config, metrics *Metrics, limiter *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if h, _, err := net.SplitHostPort(ip); err == nil {
			ip = h
		}

		if limiter.IsLimited(ip) {
			http.Error(w, "too many failed auth attempts, try again later", http.StatusTooManyRequests)
			return
		}

		// Accept token from Authorization header (preferred) or query param (backward compat).
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		} else {
			token = r.URL.Query().Get("token")
		}

		if !auth.Validate(token) {
			limiter.RecordFailure(ip)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		mode := r.URL.Query().Get("mode")
		switch mode {
		case "":
			mode = "http"
		case "http", "tcp", "udp":
		default:
			http.Error(w, "invalid tunnel mode", http.StatusBadRequest)
			return
		}

		if cfg.SingleTunnelMode && hub.Count() > 0 {
			http.Error(w, "single tunnel mode: a tunnel is already connected", http.StatusConflict)
			return
		}

		releaseSlot, capErr := hub.AcquireSlot(token, ip)
		if capErr != "" {
			log.Printf("[ws] reject %s: %s", r.RemoteAddr, capErr)
			http.Error(w, capErr, http.StatusTooManyRequests)
			return
		}
		// releaseSlot must be called when the tunnel is gone (post-Unregister) or
		// on any pre-Register failure. The defer below covers the no-Serve paths;
		// the post-Serve path explicitly calls it to keep ordering with Unregister.
		slotReleased := false
		safeRelease := func() {
			if !slotReleased {
				slotReleased = true
				releaseSlot()
			}
		}
		defer safeRelease()

		acceptOpts := &websocket.AcceptOptions{}
		if len(cfg.WSOriginPatterns) > 0 {
			acceptOpts.OriginPatterns = cfg.WSOriginPatterns
		} else {
			// Back-compat default: tunnel CLI never carries an Origin header,
			// so a strict Origin check would reject every legitimate client.
			// Set TUNNEL_WS_ORIGINS to enable the allowlist when only browser
			// clients are expected.
			acceptOpts.InsecureSkipVerify = true
		}
		conn, err := websocket.Accept(w, r, acceptOpts)
		if err != nil {
			log.Printf("[ws] accept: %v", err)
			return
		}

		conn.SetReadLimit(protocol.MaxBodySize + 4096)

		// Resolve tunnel ID. Priority:
		//   1. Explicit ?name= request (validated, atomically claimed)
		//   2. ?tunnel_id= reclaim of a recent disconnect reservation
		//   3. New random ID
		var (
			tunnelID    string
			claimedName bool
		)
		if requestedName := r.URL.Query().Get("name"); requestedName != "" {
			if !ValidName(requestedName) {
				log.Printf("[ws] invalid name %q from %s", requestedName, r.RemoteAddr)
				conn.Close(websocket.StatusPolicyViolation, "invalid name")
				return
			}
			if !hub.ClaimName(requestedName, token) {
				log.Printf("[ws] name %q already in use, rejecting %s", requestedName, r.RemoteAddr)
				conn.Close(websocket.StatusPolicyViolation, "name already in use")
				return
			}
			tunnelID = requestedName
			claimedName = true
			log.Printf("[ws] tunnel %s connected (named) from %s [mode=%s]", tunnelID, r.RemoteAddr, mode)
		} else if reclaimID := r.URL.Query().Get("tunnel_id"); reclaimID != "" && hub.ClaimReservation(reclaimID, token) {
			tunnelID = reclaimID
			log.Printf("[ws] tunnel %s reconnected (reclaimed) from %s [mode=%s]", tunnelID, r.RemoteAddr, mode)
		} else {
			var genErr error
			tunnelID, genErr = hub.GenerateID()
			if genErr != nil {
				log.Printf("[ws] %v", genErr)
				conn.CloseNow()
				return
			}
			log.Printf("[ws] tunnel %s connected from %s [mode=%s]", tunnelID, r.RemoteAddr, mode)
		}

		// If we claimed a name but later fail before Register, release the claim so
		// the name becomes available again immediately (rather than waiting for the
		// 60s reservation window — which never gets created since Unregister only
		// runs post-Register).
		releaseClaimOnFailure := func() {
			if claimedName {
				hub.ReleaseName(tunnelID, token)
			}
		}

		tunnel := NewTunnel(tunnelID, mode, conn)
		tunnel.RemoteAddr = r.RemoteAddr

		// Determine public hostname for TCP/UDP addresses.
		publicHost := cfg.PublicHost
		if publicHost == "" {
			publicHost = r.Host
			if h, _, err := net.SplitHostPort(publicHost); err == nil {
				publicHost = h
			}
		}

		scheme := "https"
		var publicURL string
		if cfg.BaseDomain != "" {
			publicURL = fmt.Sprintf("%s://%s.%s", scheme, tunnelID, cfg.BaseDomain)
		} else {
			publicURL = fmt.Sprintf("%s://%s/t/%s/", scheme, r.Host, tunnelID)
		}

		assignmentPayload := &protocol.TunnelAssignment{
			TunnelID:  tunnelID,
			PublicURL: publicURL,
			Mode:      mode,
		}
		tunnel.PublicURL = publicURL

		// Start a per-tunnel proxy for TCP/UDP modes.
		var tcpProxy *TCPProxy
		var udpProxy *UDPProxy

		switch mode {
		case "tcp":
			tcpProxy = NewTCPProxy(tunnel, metrics)
			if err := tcpProxy.Listen(":0"); err != nil {
				log.Printf("[ws] tcp listen for %s: %v", tunnelID, err)
				releaseClaimOnFailure()
				conn.CloseNow()
				return
			}
			go tcpProxy.Serve()
			port := tcpProxy.Addr().(*net.TCPAddr).Port
			assignmentPayload.TCPAddr = fmt.Sprintf("%s:%d", publicHost, port)
			tunnel.TCPAddr = assignmentPayload.TCPAddr
			log.Printf("[ws] tunnel %s: TCP proxy on port %d", tunnelID, port)

		case "udp":
			udpProxy = NewUDPProxy(tunnel, metrics)
			if err := udpProxy.Listen(":0"); err != nil {
				log.Printf("[ws] udp listen for %s: %v", tunnelID, err)
				releaseClaimOnFailure()
				conn.CloseNow()
				return
			}
			go udpProxy.Serve()
			port := udpProxy.Addr().(*net.UDPAddr).Port
			assignmentPayload.UDPAddr = fmt.Sprintf("%s:%d", publicHost, port)
			tunnel.UDPAddr = assignmentPayload.UDPAddr
			log.Printf("[ws] tunnel %s: UDP proxy on port %d", tunnelID, port)
		}

		assignment := protocol.Envelope{
			Type:       protocol.TypeAssignment,
			Assignment: assignmentPayload,
		}
		if err := tunnel.Writer.WriteEnvelopeOnly(r.Context(), assignment); err != nil {
			log.Printf("[ws] send assignment to %s: %v", tunnelID, err)
			releaseClaimOnFailure()
			if tcpProxy != nil {
				tcpProxy.Close()
			}
			if udpProxy != nil {
				udpProxy.Close()
			}
			conn.CloseNow()
			return
		}

		// Register only after successful assignment write.
		hub.Register(tunnel)
		metrics.OnTunnelConnect(mode)

		tunnel.Serve(r.Context())

		// Tunnel disconnected — clean up per-tunnel proxies.
		if tcpProxy != nil {
			tcpProxy.Close()
		}
		if udpProxy != nil {
			udpProxy.Close()
		}
		hub.Unregister(tunnelID, token)
		safeRelease()
		metrics.OnTunnelDisconnect(mode)
		conn.CloseNow()
		log.Printf("[ws] tunnel %s disconnected (reserved for 60s)", tunnelID)
	}
}
