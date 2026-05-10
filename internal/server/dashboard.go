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
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	RemoteAddr  string `json:"remote_addr"`
	ConnectedAt string `json:"connected_at"`
	Duration    string `json:"duration"`
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
		for _, t := range tunnels {
			data = append(data, dashboardTunnel{
				ID:          t.ID,
				Mode:        t.Mode,
				RemoteAddr:  t.RemoteAddr,
				ConnectedAt: t.ConnectedAt.Format(time.RFC3339),
				Duration:    now.Sub(t.ConnectedAt).Round(time.Second).String(),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tunnels": data,
			"count":   len(data),
		})
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tunnel dashboard</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, monospace; background: #0d1117; color: #c9d1d9; }
  .header { padding: 16px 24px; border-bottom: 1px solid #21262d; display: flex; align-items: center; gap: 12px; }
  .header h1 { font-size: 16px; color: #58a6ff; }
  .header .badge { background: #238636; color: #fff; font-size: 11px; padding: 2px 8px; border-radius: 10px; }
  .content { padding: 24px; max-width: 900px; }
  table { width: 100%; border-collapse: collapse; margin-top: 16px; }
  th { text-align: left; font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px; color: #8b949e; padding: 8px 12px; border-bottom: 1px solid #21262d; }
  td { padding: 10px 12px; border-bottom: 1px solid #21262d; font-size: 13px; }
  tr:hover td { background: #161b22; }
  .dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #3fb950; margin-right: 8px; }
  .mode { display: inline-block; padding: 2px 8px; border-radius: 3px; font-size: 11px; font-weight: 600; }
  .mode.http { background: #1f3a2a; color: #3fb950; }
  .mode.tcp { background: #1a2a3f; color: #58a6ff; }
  .mode.udp { background: #2a1f3a; color: #bc8cff; }
  .empty { color: #484f58; padding: 40px; text-align: center; font-style: italic; }
  .mono { font-family: monospace; color: #58a6ff; }
</style>
</head>
<body>
<div class="header">
  <h1>tunnel dashboard</h1>
  <span class="badge" id="count">0 tunnels</span>
</div>
<div class="content">
  <table>
    <thead>
      <tr><th>Status</th><th>Tunnel ID</th><th>Mode</th><th>Remote Address</th><th>Uptime</th></tr>
    </thead>
    <tbody id="tunnels">
      <tr><td colspan="5" class="empty">No active tunnels</td></tr>
    </tbody>
  </table>
</div>
<script>
function refresh() {
  fetch('/dashboard/api', { credentials: 'same-origin' })
    .then(r => r.json())
    .then(data => {
      document.getElementById('count').textContent = data.count + ' tunnel' + (data.count !== 1 ? 's' : '');
      const tb = document.getElementById('tunnels');
      if (!data.tunnels || data.tunnels.length === 0) {
        tb.innerHTML = '<tr><td colspan="5" class="empty">No active tunnels</td></tr>';
        return;
      }
      tb.innerHTML = data.tunnels.map(t =>
        '<tr>' +
        '<td><span class="dot"></span></td>' +
        '<td class="mono">' + t.id + '</td>' +
        '<td><span class="mode ' + t.mode + '">' + t.mode.toUpperCase() + '</span></td>' +
        '<td>' + t.remote_addr + '</td>' +
        '<td>' + t.duration + '</td>' +
        '</tr>'
      ).join('');
    })
    .catch(err => console.error('Failed to refresh:', err));
}
refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
