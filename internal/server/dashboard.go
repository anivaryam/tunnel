package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const dashboardCookieName = "__tunnel_dashboard"

type dashboardTunnel struct {
	ID              string `json:"id"`
	Mode            string `json:"mode"`
	RemoteAddr      string `json:"remote_addr"`
	PublicURL       string `json:"public_url"`
	TCPAddr         string `json:"tcp_addr,omitempty"`
	UDPAddr         string `json:"udp_addr,omitempty"`
	ConnectedAt     string `json:"connected_at"`
	ConnectedUnix   int64  `json:"connected_unix"`
	Duration        string `json:"duration"`
	ActiveStreams   int    `json:"active_streams"`
	PendingRequests int    `json:"pending_requests"`
	WSStreams       int    `json:"ws_streams"`
	SSEStreams      int    `json:"sse_streams"`
}

// dashboardToken extracts the dashboard auth token in priority order:
// HttpOnly session cookie (preferred), Authorization: Bearer header, or
// the legacy ?token= query parameter (kept for bookmark backward-compat).
func dashboardToken(r *http.Request) string {
	if c, err := r.Cookie(dashboardCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// DashboardHandler serves the admin dashboard. When ?token=X is provided we
// validate it, set an HttpOnly session cookie, and redirect to /dashboard so
// the token never appears in browser history, Referer headers, or access logs
// for follow-up requests.
func DashboardHandler(_ *Hub, auth *Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		queryToken := r.URL.Query().Get("token")
		if queryToken != "" {
			if !auth.Validate(queryToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     dashboardCookieName,
				Value:    queryToken,
				Path:     "/dashboard",
				HttpOnly: true,
				Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
				SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		token := dashboardToken(r)
		if !auth.Validate(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, dashboardHTML)
	}
}

// DashboardAPIHandler returns tunnel data as JSON.
func DashboardAPIHandler(hub *Hub, auth *Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := dashboardToken(r)
		if !auth.Validate(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		tunnels := hub.List()
		data := make([]dashboardTunnel, 0, len(tunnels))
		now := time.Now()
		byMode := map[string]int{"http": 0, "tcp": 0, "udp": 0}
		totalStreams := 0
		for _, t := range tunnels {
			stats := t.Stats()
			byMode[t.Mode]++
			totalStreams += stats.ActiveStreams + stats.WSStreams + stats.SSEStreams
			data = append(data, dashboardTunnel{
				ID:              t.ID,
				Mode:            t.Mode,
				RemoteAddr:      t.RemoteAddr,
				PublicURL:       t.PublicURL,
				TCPAddr:         t.TCPAddr,
				UDPAddr:         t.UDPAddr,
				ConnectedAt:     t.ConnectedAt.Format(time.RFC3339),
				ConnectedUnix:   t.ConnectedAt.Unix(),
				Duration:        now.Sub(t.ConnectedAt).Round(time.Second).String(),
				ActiveStreams:   stats.ActiveStreams,
				PendingRequests: stats.PendingRequests,
				WSStreams:       stats.WSStreams,
				SSEStreams:      stats.SSEStreams,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tunnels":       data,
			"count":         len(data),
			"by_mode":       byMode,
			"total_streams": totalStreams,
			"server_time":   now.Format(time.RFC3339),
		})
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>TUNNEL // RELAY CONTROL</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Orbitron:wght@500;700;900&family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet">
<style>
  :root {
    --bg-0: #04060b;
    --bg-1: #080d18;
    --bg-2: #0c1322;
    --line: rgba(118, 230, 255, 0.12);
    --line-strong: rgba(118, 230, 255, 0.32);
    --fg: #d6e6ff;
    --fg-dim: #6a7e9a;
    --accent: #00f0ff;
    --accent-2: #ff2bd6;
    --accent-3: #b8ff5e;
    --warn: #ffb547;
    --shadow-glow: 0 0 24px rgba(0, 240, 255, 0.18), 0 0 80px rgba(255, 43, 214, 0.06);
  }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { background: var(--bg-0); color: var(--fg); font-family: 'JetBrains Mono', ui-monospace, monospace; min-height: 100dvh; }
  body {
    background:
      radial-gradient(1200px 600px at 80% -10%, rgba(255,43,214,0.10), transparent 60%),
      radial-gradient(1000px 500px at -10% 100%, rgba(0,240,255,0.10), transparent 60%),
      linear-gradient(180deg, #04060b 0%, #060912 100%);
    overflow-x: hidden;
    -webkit-font-smoothing: antialiased;
  }
  #bg { position: fixed; inset: 0; z-index: 0; pointer-events: none; }
  #bg canvas { width: 100%; height: 100%; display: block; opacity: 0.55; }
  .scanlines {
    position: fixed; inset: 0; z-index: 1; pointer-events: none; mix-blend-mode: overlay;
    background: repeating-linear-gradient(to bottom, rgba(255,255,255,0.025) 0 1px, transparent 1px 3px);
    opacity: 0.6;
  }
  .vignette { position: fixed; inset: 0; z-index: 1; pointer-events: none; box-shadow: inset 0 0 220px 40px rgba(0,0,0,0.65); }
  main { position: relative; z-index: 2; max-width: 1400px; margin: 0 auto; padding: 24px clamp(16px, 4vw, 40px) 80px; }

  .topbar {
    display: flex; align-items: center; justify-content: space-between; gap: 16px;
    padding: 14px 18px; margin-bottom: 24px;
    border: 1px solid var(--line); border-radius: 14px;
    background: linear-gradient(180deg, rgba(8,13,24,0.85), rgba(8,13,24,0.55));
    backdrop-filter: blur(14px) saturate(140%);
    -webkit-backdrop-filter: blur(14px) saturate(140%);
    box-shadow: var(--shadow-glow);
  }
  .brand { display: flex; align-items: center; gap: 12px; min-width: 0; }
  .logo {
    width: 36px; height: 36px; flex: 0 0 36px;
    border: 1px solid var(--line-strong); border-radius: 10px;
    display: grid; place-items: center;
    background: radial-gradient(circle at 30% 30%, rgba(0,240,255,0.35), transparent 60%);
    box-shadow: inset 0 0 12px rgba(0,240,255,0.35);
  }
  .logo svg { width: 20px; height: 20px; }
  .brand-text { display: flex; flex-direction: column; min-width: 0; }
  .brand h1 {
    font-family: 'Orbitron', sans-serif; font-weight: 900; font-size: 15px; letter-spacing: 0.28em;
    color: var(--fg); text-transform: uppercase; line-height: 1;
  }
  .brand small {
    font-size: 10px; letter-spacing: 0.32em; color: var(--fg-dim); text-transform: uppercase; margin-top: 4px;
  }
  .status-cluster { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; justify-content: flex-end; }
  .pill {
    display: inline-flex; align-items: center; gap: 8px;
    padding: 6px 12px; border-radius: 999px; font-size: 11px; letter-spacing: 0.18em; text-transform: uppercase;
    border: 1px solid var(--line); background: rgba(0,240,255,0.04); color: var(--fg);
  }
  .pill .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--accent-3); box-shadow: 0 0 10px var(--accent-3); animation: pulse 2s infinite; }
  .pill .dot.offline { background: var(--warn); box-shadow: 0 0 10px var(--warn); }
  .clock { font-family: 'Orbitron', sans-serif; font-size: 12px; letter-spacing: 0.22em; color: var(--accent); text-shadow: 0 0 12px rgba(0,240,255,0.6); }

  .kpis { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 24px; }
  .kpi {
    position: relative; overflow: hidden;
    padding: 16px 18px; border: 1px solid var(--line); border-radius: 14px;
    background: linear-gradient(180deg, rgba(12,19,34,0.85), rgba(12,19,34,0.55));
    backdrop-filter: blur(14px); -webkit-backdrop-filter: blur(14px);
  }
  .kpi::before {
    content: ""; position: absolute; inset: 0; pointer-events: none;
    background: linear-gradient(120deg, transparent 30%, rgba(0,240,255,0.08) 50%, transparent 70%);
    transform: translateX(-100%); transition: transform .8s;
  }
  .kpi:hover::before { transform: translateX(100%); }
  .kpi h3 { font-size: 10px; letter-spacing: 0.32em; color: var(--fg-dim); text-transform: uppercase; }
  .kpi .v { font-family: 'Orbitron', sans-serif; font-weight: 700; font-size: clamp(22px, 3.4vw, 30px); color: var(--fg); margin-top: 8px; line-height: 1; }
  .kpi.cyan .v { color: var(--accent); text-shadow: 0 0 16px rgba(0,240,255,0.55); }
  .kpi.magenta .v { color: var(--accent-2); text-shadow: 0 0 16px rgba(255,43,214,0.55); }
  .kpi.lime .v { color: var(--accent-3); text-shadow: 0 0 16px rgba(184,255,94,0.45); }
  .kpi .sub { font-size: 10px; letter-spacing: 0.2em; color: var(--fg-dim); margin-top: 6px; text-transform: uppercase; }

  .section-head { display: flex; align-items: baseline; justify-content: space-between; gap: 16px; margin: 8px 4px 12px; }
  .section-head h2 { font-family: 'Orbitron', sans-serif; font-weight: 700; font-size: 13px; letter-spacing: 0.32em; color: var(--fg); text-transform: uppercase; }
  .section-head .meta { font-size: 11px; letter-spacing: 0.2em; color: var(--fg-dim); text-transform: uppercase; }

  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 14px; }
  .card {
    position: relative; padding: 16px; border: 1px solid var(--line); border-radius: 14px;
    background: linear-gradient(180deg, rgba(10,16,28,0.9), rgba(10,16,28,0.6));
    backdrop-filter: blur(14px); -webkit-backdrop-filter: blur(14px);
    transition: border-color .2s, transform .2s, box-shadow .2s;
  }
  .card:hover { border-color: var(--line-strong); transform: translateY(-1px); box-shadow: 0 0 24px rgba(0,240,255,0.08); }
  .card .corner { position: absolute; width: 12px; height: 12px; border: 1px solid var(--accent); opacity: 0.7; }
  .card .corner.tl { top: -1px; left: -1px; border-right: none; border-bottom: none; }
  .card .corner.tr { top: -1px; right: -1px; border-left: none; border-bottom: none; }
  .card .corner.bl { bottom: -1px; left: -1px; border-right: none; border-top: none; }
  .card .corner.br { bottom: -1px; right: -1px; border-left: none; border-top: none; }

  .card-head { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
  .card-head .live { width: 8px; height: 8px; border-radius: 50%; background: var(--accent-3); box-shadow: 0 0 10px var(--accent-3); animation: pulse 1.6s infinite; flex: 0 0 8px; }
  .card-head .id { font-family: 'Orbitron', sans-serif; font-weight: 700; font-size: 14px; letter-spacing: 0.16em; color: var(--fg); flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .badge {
    font-size: 10px; letter-spacing: 0.2em; text-transform: uppercase;
    padding: 4px 10px; border-radius: 999px; border: 1px solid;
  }
  .badge.http { color: var(--accent); border-color: rgba(0,240,255,0.35); background: rgba(0,240,255,0.08); }
  .badge.tcp { color: var(--accent-2); border-color: rgba(255,43,214,0.35); background: rgba(255,43,214,0.08); }
  .badge.udp { color: var(--accent-3); border-color: rgba(184,255,94,0.35); background: rgba(184,255,94,0.08); }

  .url-row {
    display: flex; align-items: center; gap: 8px;
    padding: 10px 12px; margin-bottom: 12px;
    border: 1px dashed var(--line-strong); border-radius: 10px;
    background: rgba(0,240,255,0.03);
  }
  .url-row a { color: var(--accent); text-decoration: none; font-size: 12px; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .url-row a:hover { text-decoration: underline; }
  .copy {
    background: transparent; border: 1px solid var(--line); color: var(--fg-dim);
    padding: 4px 8px; border-radius: 6px; font-family: inherit; font-size: 10px; letter-spacing: 0.16em;
    cursor: pointer; text-transform: uppercase; transition: all .15s;
  }
  .copy:hover { color: var(--accent); border-color: var(--accent); }
  .copy.ok { color: var(--accent-3); border-color: var(--accent-3); }

  .specs { display: grid; grid-template-columns: 1fr 1fr; gap: 8px 14px; margin-bottom: 12px; }
  .spec { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
  .spec dt { font-size: 9px; letter-spacing: 0.24em; color: var(--fg-dim); text-transform: uppercase; }
  .spec dd { font-size: 12px; color: var(--fg); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .spec dd.uptime { color: var(--accent); font-family: 'Orbitron', sans-serif; letter-spacing: 0.08em; }

  .stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 6px; padding-top: 10px; border-top: 1px solid var(--line); }
  .stat { display: flex; flex-direction: column; align-items: center; gap: 4px; padding: 6px 4px; border-radius: 8px; background: rgba(118,230,255,0.03); }
  .stat .n { font-family: 'Orbitron', sans-serif; font-weight: 700; font-size: 16px; color: var(--fg); line-height: 1; }
  .stat .l { font-size: 8px; letter-spacing: 0.2em; color: var(--fg-dim); text-transform: uppercase; text-align: center; }
  .stat.hot .n { color: var(--accent); text-shadow: 0 0 8px rgba(0,240,255,0.5); }

  .empty {
    grid-column: 1 / -1;
    padding: 60px 20px; text-align: center; color: var(--fg-dim);
    border: 1px dashed var(--line); border-radius: 14px;
    font-size: 13px; letter-spacing: 0.16em; text-transform: uppercase;
  }
  .empty .blink { color: var(--accent); animation: blink 1.2s steps(2, start) infinite; }

  @keyframes pulse { 0%,100% { opacity: 1; transform: scale(1); } 50% { opacity: 0.55; transform: scale(0.85); } }
  @keyframes blink { 50% { opacity: 0; } }

  @media (max-width: 720px) {
    .kpis { grid-template-columns: repeat(2, 1fr); }
    .brand h1 { font-size: 13px; letter-spacing: 0.22em; }
    .topbar { flex-direction: column; align-items: stretch; }
    .status-cluster { justify-content: space-between; }
    .grid { grid-template-columns: 1fr; }
    .specs { grid-template-columns: 1fr; }
  }

  @media (prefers-reduced-motion: reduce) {
    #bg { display: none; }
    .pill .dot, .card-head .live, .empty .blink, .kpi::before { animation: none; }
  }
</style>
</head>
<body>
<div id="bg"><canvas id="bgcanvas"></canvas></div>
<div class="scanlines"></div>
<div class="vignette"></div>

<main>
  <header class="topbar">
    <div class="brand">
      <div class="logo" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="#00f0ff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M2 12h6l3-9 3 18 3-9h5"/>
        </svg>
      </div>
      <div class="brand-text">
        <h1>Tunnel</h1>
        <small>Relay Control // v1</small>
      </div>
    </div>
    <div class="status-cluster">
      <span class="pill" id="link-pill"><span class="dot"></span><span>Link Online</span></span>
      <span class="clock" id="clock" aria-label="server time">--:--:--</span>
    </div>
  </header>

  <section class="kpis" aria-label="summary">
    <div class="kpi cyan"><h3>Active Tunnels</h3><div class="v" id="kpi-total">0</div><div class="sub" id="kpi-server">— sync</div></div>
    <div class="kpi magenta"><h3>HTTP</h3><div class="v" id="kpi-http">0</div><div class="sub">L7 sessions</div></div>
    <div class="kpi"><h3>TCP / UDP</h3><div class="v" id="kpi-l4">0</div><div class="sub">L4 sessions</div></div>
    <div class="kpi lime"><h3>Live Streams</h3><div class="v" id="kpi-streams">0</div><div class="sub">aggregate</div></div>
  </section>

  <div class="section-head">
    <h2>Channels</h2>
    <span class="meta" id="last-sync">awaiting sync…</span>
  </div>
  <div class="grid" id="tunnels">
    <div class="empty">No active tunnels — <span class="blink">_</span> listening for handshake</div>
  </div>
</main>

<script>
(() => {
  const canvas = document.getElementById('bgcanvas');
  const ctx = canvas.getContext('2d', { alpha: true });
  let w = 0, h = 0, dpr = Math.min(window.devicePixelRatio || 1, 2);
  let t0 = performance.now();
  const NODES = 48;
  const nodes = Array.from({length: NODES}, () => ({
    x: Math.random(), y: Math.random(),
    vx: (Math.random() - 0.5) * 0.00012,
    vy: (Math.random() - 0.5) * 0.00012,
  }));

  function resize() {
    const rect = canvas.parentElement.getBoundingClientRect();
    w = rect.width; h = rect.height;
    canvas.width = Math.floor(w * dpr);
    canvas.height = Math.floor(h * dpr);
    canvas.style.width = w + 'px';
    canvas.style.height = h + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }
  resize();
  window.addEventListener('resize', resize);

  function frame(now) {
    const dt = now - t0; t0 = now;
    ctx.clearRect(0, 0, w, h);

    // grid
    ctx.strokeStyle = 'rgba(0,240,255,0.06)';
    ctx.lineWidth = 1;
    const step = 60;
    const off = (now * 0.012) % step;
    ctx.beginPath();
    for (let x = -step + off; x < w + step; x += step) { ctx.moveTo(x, 0); ctx.lineTo(x + (h * 0.15), h); }
    for (let y = -step + off; y < h + step; y += step) { ctx.moveTo(0, y); ctx.lineTo(w, y - (w * 0.05)); }
    ctx.stroke();

    // nodes + links
    for (const n of nodes) {
      n.x += n.vx * dt; n.y += n.vy * dt;
      if (n.x < 0 || n.x > 1) n.vx *= -1;
      if (n.y < 0 || n.y > 1) n.vy *= -1;
    }
    for (let i = 0; i < nodes.length; i++) {
      const a = nodes[i];
      for (let j = i + 1; j < nodes.length; j++) {
        const b = nodes[j];
        const dx = (a.x - b.x) * w, dy = (a.y - b.y) * h;
        const d2 = dx*dx + dy*dy;
        if (d2 < 160*160) {
          const alpha = (1 - Math.sqrt(d2)/160) * 0.35;
          ctx.strokeStyle = 'rgba(0,240,255,' + alpha.toFixed(3) + ')';
          ctx.beginPath();
          ctx.moveTo(a.x * w, a.y * h);
          ctx.lineTo(b.x * w, b.y * h);
          ctx.stroke();
        }
      }
    }
    for (const n of nodes) {
      ctx.fillStyle = 'rgba(184,255,94,0.55)';
      ctx.beginPath(); ctx.arc(n.x * w, n.y * h, 1.4, 0, Math.PI * 2); ctx.fill();
    }
    requestAnimationFrame(frame);
  }
  if (!window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    requestAnimationFrame(frame);
  }
})();

const clock = document.getElementById('clock');
setInterval(() => {
  const d = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  clock.textContent = pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()) + ':' + pad(d.getUTCSeconds()) + ' UTC';
}, 1000);

const escapeHTML = (s) => String(s == null ? '' : s)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;').replace(/'/g, '&#39;');

function fmtUptime(unix) {
  if (!unix) return '—';
  let s = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  const d = Math.floor(s / 86400); s %= 86400;
  const h = Math.floor(s / 3600); s %= 3600;
  const m = Math.floor(s / 60); s %= 60;
  const pad = (n) => String(n).padStart(2, '0');
  if (d > 0) return d + 'd ' + pad(h) + ':' + pad(m) + ':' + pad(s);
  return pad(h) + ':' + pad(m) + ':' + pad(s);
}

function copy(btn, text) {
  navigator.clipboard.writeText(text).then(() => {
    btn.textContent = 'copied';
    btn.classList.add('ok');
    setTimeout(() => { btn.textContent = 'copy'; btn.classList.remove('ok'); }, 1400);
  }).catch(() => { btn.textContent = 'err'; });
}

let cache = [];

function render(list) {
  const tb = document.getElementById('tunnels');
  if (!list || list.length === 0) {
    tb.innerHTML = '<div class="empty">No active tunnels — <span class="blink">_</span> listening for handshake</div>';
    return;
  }
  tb.innerHTML = list.map(t => {
    const mode = t.mode || 'http';
    const url = t.public_url || '';
    const addrLine = t.tcp_addr || t.udp_addr || url || '—';
    const remote = t.remote_addr || '—';
    return ''
      + '<article class="card" data-id="' + escapeHTML(t.id) + '">'
      + '<span class="corner tl"></span><span class="corner tr"></span><span class="corner bl"></span><span class="corner br"></span>'
      + '<div class="card-head">'
      +   '<span class="live" aria-hidden="true"></span>'
      +   '<span class="id">' + escapeHTML(t.id) + '</span>'
      +   '<span class="badge ' + mode + '">' + mode.toUpperCase() + '</span>'
      + '</div>'
      + (url
          ? '<div class="url-row"><a href="' + escapeHTML(url) + '" target="_blank" rel="noopener">' + escapeHTML(url) + '</a><button class="copy" data-copy="' + escapeHTML(url) + '">copy</button></div>'
          : '<div class="url-row"><a>' + escapeHTML(addrLine) + '</a><button class="copy" data-copy="' + escapeHTML(addrLine) + '">copy</button></div>')
      + '<dl class="specs">'
      +   '<div class="spec"><dt>Remote IP</dt><dd title="' + escapeHTML(remote) + '">' + escapeHTML(remote) + '</dd></div>'
      +   '<div class="spec"><dt>Uptime</dt><dd class="uptime" data-uptime="' + (t.connected_unix||0) + '">' + fmtUptime(t.connected_unix) + '</dd></div>'
      +   '<div class="spec"><dt>Connected</dt><dd>' + escapeHTML(new Date((t.connected_unix||0)*1000).toLocaleString()) + '</dd></div>'
      +   '<div class="spec"><dt>Endpoint</dt><dd title="' + escapeHTML(addrLine) + '">' + escapeHTML(addrLine) + '</dd></div>'
      + '</dl>'
      + '<div class="stats">'
      +   '<div class="stat ' + (t.active_streams ? 'hot' : '') + '"><span class="n">' + (t.active_streams||0) + '</span><span class="l">Streams</span></div>'
      +   '<div class="stat ' + (t.pending_requests ? 'hot' : '') + '"><span class="n">' + (t.pending_requests||0) + '</span><span class="l">Pending</span></div>'
      +   '<div class="stat ' + (t.ws_streams ? 'hot' : '') + '"><span class="n">' + (t.ws_streams||0) + '</span><span class="l">WS</span></div>'
      +   '<div class="stat ' + (t.sse_streams ? 'hot' : '') + '"><span class="n">' + (t.sse_streams||0) + '</span><span class="l">SSE</span></div>'
      + '</div>'
      + '</article>';
  }).join('');
  tb.querySelectorAll('.copy').forEach(btn => {
    btn.addEventListener('click', () => copy(btn, btn.dataset.copy));
  });
}

setInterval(() => {
  document.querySelectorAll('[data-uptime]').forEach(el => {
    el.textContent = fmtUptime(parseInt(el.dataset.uptime, 10));
  });
}, 1000);

function setOnline(ok) {
  const pill = document.getElementById('link-pill');
  const dot = pill.querySelector('.dot');
  const label = pill.querySelector('span:last-child');
  dot.classList.toggle('offline', !ok);
  label.textContent = ok ? 'Link Online' : 'Link Offline';
}

function refresh() {
  fetch('/dashboard/api', { credentials: 'same-origin' })
    .then(r => { if (!r.ok) throw new Error('http ' + r.status); return r.json(); })
    .then(data => {
      setOnline(true);
      cache = data.tunnels || [];
      document.getElementById('kpi-total').textContent = data.count || 0;
      const byMode = data.by_mode || {};
      document.getElementById('kpi-http').textContent = byMode.http || 0;
      document.getElementById('kpi-l4').textContent = (byMode.tcp || 0) + (byMode.udp || 0);
      document.getElementById('kpi-streams').textContent = data.total_streams || 0;
      document.getElementById('kpi-server').textContent = (data.server_time || '').replace('T', ' ').replace(/\..*/, '');
      document.getElementById('last-sync').textContent = 'synced ' + new Date().toLocaleTimeString();
      render(cache);
    })
    .catch(err => { setOnline(false); console.error('refresh failed:', err); });
}
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
