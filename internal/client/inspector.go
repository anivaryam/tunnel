package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const maxEntries = 500

// InspectEntry represents a captured request/response pair.
type InspectEntry struct {
	ID              string              `json:"id"`
	Timestamp       time.Time           `json:"timestamp"`
	Method          string              `json:"method"`
	Path            string              `json:"path"`
	Query           string              `json:"query,omitempty"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	RequestBody     string              `json:"request_body,omitempty"`
	StatusCode      int                 `json:"status_code"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	ResponseBody    string              `json:"response_body,omitempty"`
	Duration        time.Duration       `json:"duration_ns"`
	DurationStr     string              `json:"duration"`
}

// Inspector captures request/response pairs and serves a web UI.
type Inspector struct {
	mu        sync.RWMutex
	entries   []InspectEntry
	listeners []chan InspectEntry
}

// NewInspector creates a new Inspector.
func NewInspector() *Inspector {
	return &Inspector{}
}

// Add records an entry and notifies SSE listeners.
func (ins *Inspector) Add(e InspectEntry) {
	ins.mu.Lock()
	ins.entries = append(ins.entries, e)
	if len(ins.entries) > maxEntries {
		ins.entries = ins.entries[len(ins.entries)-maxEntries:]
	}
	listeners := make([]chan InspectEntry, len(ins.listeners))
	copy(listeners, ins.listeners)
	ins.mu.Unlock()

	for _, ch := range listeners {
		select {
		case ch <- e:
		default:
		}
	}
}

func (ins *Inspector) subscribe() chan InspectEntry {
	ch := make(chan InspectEntry, 16)
	ins.mu.Lock()
	ins.listeners = append(ins.listeners, ch)
	ins.mu.Unlock()
	return ch
}

func (ins *Inspector) unsubscribe(ch chan InspectEntry) {
	ins.mu.Lock()
	defer ins.mu.Unlock()
	for i, l := range ins.listeners {
		if l == ch {
			ins.listeners = append(ins.listeners[:i], ins.listeners[i+1:]...)
			break
		}
	}
}

// Start launches the inspector web server on the given address. Returns
// (true, "") if the listener was bound successfully; (false, errMsg)
// otherwise so the caller can omit the inspector URL from banners.
func (ins *Inspector) Start(addr string) (bool, string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ins.handleUI)
	mux.HandleFunc("/api/requests", ins.handleAPI)
	mux.HandleFunc("/api/requests/stream", ins.handleSSE)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false, err.Error()
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		log.Printf("inspector web UI: http://%s", ln.Addr())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("inspector error: %v", err)
		}
	}()
	return true, ""
}

func (ins *Inspector) handleAPI(w http.ResponseWriter, _ *http.Request) {
	ins.mu.RLock()
	entries := make([]InspectEntry, len(ins.entries))
	copy(entries, ins.entries)
	ins.mu.RUnlock()

	// Reverse so newest is first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (ins *Inspector) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch := ins.subscribe()
	defer ins.unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case entry := <-ch:
			data, _ := json.Marshal(entry)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (ins *Inspector) handleUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, inspectorHTML)
}

const inspectorHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tunnel inspector</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, monospace; background: #0d1117; color: #c9d1d9; }
  .header { padding: 16px 24px; border-bottom: 1px solid #21262d; display: flex; align-items: center; gap: 12px; }
  .header h1 { font-size: 16px; color: #58a6ff; }
  .header .badge { background: #238636; color: #fff; font-size: 11px; padding: 2px 8px; border-radius: 10px; }
  .container { display: flex; height: calc(100vh - 53px); }
  .list { width: 420px; min-width: 420px; border-right: 1px solid #21262d; overflow-y: auto; }
  .detail { flex: 1; overflow-y: auto; padding: 20px; }
  .entry { padding: 10px 16px; border-bottom: 1px solid #21262d; cursor: pointer; display: flex; align-items: center; gap: 10px; }
  .entry:hover { background: #161b22; }
  .entry.selected { background: #1c2128; border-left: 3px solid #58a6ff; }
  .method { font-weight: 700; font-size: 12px; width: 56px; text-align: center; padding: 2px 0; border-radius: 3px; }
  .method.GET { color: #3fb950; }
  .method.POST { color: #d29922; }
  .method.PUT { color: #58a6ff; }
  .method.DELETE { color: #f85149; }
  .method.PATCH { color: #bc8cff; }
  .path { flex: 1; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .status { font-size: 12px; font-weight: 600; }
  .status.s2xx { color: #3fb950; }
  .status.s3xx { color: #d29922; }
  .status.s4xx { color: #f85149; }
  .status.s5xx { color: #f85149; }
  .time { font-size: 11px; color: #484f58; }
  .duration { font-size: 11px; color: #484f58; width: 60px; text-align: right; }
  .section { margin-bottom: 20px; }
  .section h3 { font-size: 13px; color: #8b949e; text-transform: uppercase; letter-spacing: 0.5px; margin-bottom: 8px; padding-bottom: 6px; border-bottom: 1px solid #21262d; }
  .headers-table { width: 100%; font-size: 13px; }
  .headers-table td { padding: 3px 0; vertical-align: top; }
  .headers-table td:first-child { color: #58a6ff; width: 200px; padding-right: 12px; }
  .body-block { background: #161b22; border: 1px solid #21262d; border-radius: 6px; padding: 12px; font-size: 13px; white-space: pre-wrap; word-break: break-all; max-height: 400px; overflow-y: auto; }
  .empty { color: #484f58; font-style: italic; padding: 40px; text-align: center; }
  .detail .empty { padding: 100px 40px; }
  .status-line { font-size: 20px; font-weight: 700; margin-bottom: 16px; display: flex; align-items: center; gap: 10px; }
</style>
</head>
<body>
<div class="header">
  <h1>tunnel inspector</h1>
  <span class="badge" id="count">0 requests</span>
</div>
<div class="container">
  <div class="list" id="list">
    <div class="empty">Waiting for requests...</div>
  </div>
  <div class="detail" id="detail">
    <div class="empty">Select a request to inspect</div>
  </div>
</div>
<script>
let entries = [];
let selected = null;

function statusClass(code) {
  if (code >= 200 && code < 300) return 's2xx';
  if (code >= 300 && code < 400) return 's3xx';
  if (code >= 400 && code < 500) return 's4xx';
  return 's5xx';
}

function timeStr(ts) {
  return new Date(ts).toLocaleTimeString();
}

function renderList() {
  const el = document.getElementById('list');
  if (entries.length === 0) {
    el.innerHTML = '<div class="empty">Waiting for requests...</div>';
    return;
  }
  el.innerHTML = entries.map((e, i) => {
    const sc = statusClass(e.status_code);
    const sel = selected === i ? ' selected' : '';
    return '<div class="entry' + sel + '" onclick="select(' + i + ')">' +
      '<span class="method ' + e.method + '">' + e.method + '</span>' +
      '<span class="path">' + e.path + (e.query ? '?' + e.query : '') + '</span>' +
      '<span class="status ' + sc + '">' + e.status_code + '</span>' +
      '<span class="duration">' + e.duration + '</span>' +
      '</div>';
  }).join('');
  document.getElementById('count').textContent = entries.length + ' request' + (entries.length !== 1 ? 's' : '');
}

function headersHTML(h) {
  if (!h || Object.keys(h).length === 0) return '<span class="empty">No headers</span>';
  let s = '<table class="headers-table">';
  for (const [k, vals] of Object.entries(h)) {
    s += '<tr><td>' + k + '</td><td>' + vals.join(', ') + '</td></tr>';
  }
  return s + '</table>';
}

function bodyHTML(b) {
  if (!b) return '<span class="empty">Empty body</span>';
  try { return JSON.stringify(JSON.parse(b), null, 2); } catch(e) { return b; }
}

window.select = function(i) {
  selected = i;
  renderList();
  const e = entries[i];
  const sc = statusClass(e.status_code);
  document.getElementById('detail').innerHTML =
    '<div class="status-line"><span class="method ' + e.method + '">' + e.method + '</span> ' +
    '<span>' + e.path + (e.query ? '?' + e.query : '') + '</span>' +
    '<span class="status ' + sc + '">' + e.status_code + '</span>' +
    '<span class="duration">' + e.duration + '</span></div>' +
    '<div class="section"><h3>Request Headers</h3>' + headersHTML(e.request_headers) + '</div>' +
    '<div class="section"><h3>Request Body</h3><div class="body-block">' + bodyHTML(e.request_body) + '</div></div>' +
    '<div class="section"><h3>Response Headers</h3>' + headersHTML(e.response_headers) + '</div>' +
    '<div class="section"><h3>Response Body</h3><div class="body-block">' + bodyHTML(e.response_body) + '</div></div>';
};

fetch('/api/requests').then(r => r.json()).then(data => {
  entries = data || [];
  renderList();
});

const es = new EventSource('/api/requests/stream');
es.onmessage = function(ev) {
  const e = JSON.parse(ev.data);
  entries.unshift(e);
  if (entries.length > 500) entries.pop();
  if (selected !== null) selected++;
  renderList();
};
</script>
</body>
</html>`
