# tunnel

A self-hosted ngrok alternative. Expose local servers to the internet through a relay server you control.

```
Browser → Relay Server → WebSocket → CLI Client → localhost
```

## Features

- **HTTP tunneling** with path-based or subdomain routing
- **Multi-tunnel isolation** — run multiple apps simultaneously without cross-contamination
- **Single tunnel mode** — lock the relay to one tunnel; root domain proxies directly with no path prefix
- **Cloudflare Worker subdomain routing** — free wildcard subdomains even on Railway's free tier
- **Named tunnels** — stable human-readable URLs via `--name`
- **Vite / SPA support** — automatic WebSocket and fetch rewrite for HMR and API calls
- **TCP tunneling** — for databases, SSH, etc. (per-tunnel dynamic ports)
- **UDP tunneling** — for DNS, game servers, etc. (per-tunnel dynamic ports)
- **Request inspector** — local web UI at `:4040`
- **Auto-reconnection** with tunnel ID preservation
- **Admin dashboard** at `/dashboard` on the relay
- **Prometheus metrics** at `/metrics`
- **Cross-platform** — Linux, macOS, Windows (amd64/arm64)
- **Daemon mode** — `--silent` background mode, `tunnel daemon monitor|status|stop`, size-rotated logs (release binaries and `make install` ship it; opt out with `make install-no-daemon`)
- **Diagnostics** — `tunnel doctor` walks setup, `tunnel list` enumerates active tunnels, `tunnel config show` masks tokens
- **Signed releases** — checksums + cosign keyless signing (Sigstore Rekor tlog)

## Install

**With [brokit](https://github.com/anivaryam/brokit)** (recommended if you use multiple tools from this org — handles install + update + uninstall):

```sh
brokit install tunnel              # install latest release
brokit update tunnel               # upgrade to latest
brokit list                        # see installed tools and versions
brokit remove tunnel               # uninstall
```

`brokit` reads the GitHub releases for this repo, verifies sha256, and drops the binary into `/usr/local/bin`. Daemon mode and man pages are included.

**From release binary (Linux/macOS, single-tool install):**

```sh
curl -sSL https://raw.githubusercontent.com/anivaryam/tunnel/main/install.sh | sh
```

The installer fetches the matching release archive, verifies its sha256 against `checksums.txt`, extracts the binary into `/usr/local/bin/tunnel`, and unpacks man pages alongside it.

**From source:**

```sh
git clone https://github.com/anivaryam/tunnel.git
cd tunnel
make install                  # builds with -tags daemon (default; includes --silent, daemon monitor/stop)
make install-no-daemon        # smaller binary without daemon mode
```

**With Go** (skips daemon mode and man pages — release binary recommended for end-user installs):

```sh
go install github.com/anivaryam/tunnel/cmd/tunnel@latest
```

### Verify a release

Each release ships `checksums.txt`, `checksums.txt.sig`, and `checksums.txt.pem` (cosign keyless via GitHub Actions OIDC). Verify the bundle before extracting:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/anivaryam/tunnel/.*' \
  --certificate-oidc-issuer     'https://token.actions.githubusercontent.com' \
  checksums.txt
```

Then `sha256sum -c checksums.txt` against the downloaded archives.

### Install man pages

Release archives bundle `man/tunnel*.1`. After extracting:

```sh
install -m 644 man/tunnel*.1 ~/.local/share/man/man1/
man tunnel
```

## Quick Start

```sh
# Configure once
tunnel config set-server https://relay.example.com
tunnel config set-token   your-secret-token
tunnel config show                # confirm (token is masked)

# Sanity-check the setup before exposing anything
tunnel doctor

# Expose a local HTTP server
tunnel http 3000
# → https://relay.example.com/t/<random-16-char-id>/

# Expose with a stable name (and subdomain when BASE_DOMAIN is set on the relay)
tunnel http 3000 --name myapp
# → https://relay.example.com/t/myapp/         (path-based)
# → https://myapp.relay.example.com/           (subdomain, if BASE_DOMAIN configured)

# Expose with request inspector
tunnel http 3000 --inspect       # http://127.0.0.1:4040

# Expose TCP / UDP services
tunnel tcp 5432                  # Postgres, SSH, etc.
tunnel udp 53                    # DNS, game servers, etc.

# Run in background (release binary / `make install`)
tunnel http 3000 --name api --silent
tunnel daemon status  --name api          # human snapshot
tunnel daemon status  --name api --json   # machine-readable
tunnel daemon monitor --name api          # interactive TUI
tunnel daemon stop    --name api          # graceful shutdown

# Enumerate live tunnels (queries the relay's dashboard API)
tunnel list                       # table
tunnel list --json                # script-friendly
```

`tunnel monitor|status|stop` (top-level, without `daemon`) still work — both forms are supported.

For framework-specific setup tips (CORS, cookies, WebSockets, asset paths, redirects), see [Optimizing Local Projects for tunnel](docs/guides/optimizing-local-projects-for-tunnel.md).

## CLI Usage

```
tunnel http <port> [flags]        Create an HTTP tunnel
tunnel tcp  <port> [flags]        Create a TCP tunnel
tunnel udp  <port> [flags]        Create a UDP tunnel

Configuration:
  tunnel config set-server <url>  Set relay server URL (validates scheme + host)
  tunnel config set-token  <tok>  Set auth token (rejects empty)
  tunnel config show              Print current config (token masked)

Diagnostic & introspection:
  tunnel doctor                   Walk config, perms, URL parse, relay reachability, token
  tunnel list [--json]            Enumerate active tunnels via the relay's dashboard API

Daemon mode (release binary or `make install`):
  -s, --silent                    Re-exec detached and exit. Parent waits up to 3s
                                  for the child to reach state=connected and warns
                                  if it doesn't (bad URL, bad token, network down).
  --log-file string               Daemon log path (default: $XDG_RUNTIME_DIR/tunnel/tunnel-<hash>.log)
  --max-log-size int              Rotate log when it exceeds N bytes (0 = no rotation)

  tunnel daemon stop    [--name X | --port N --mode http|tcp|udp]   Graceful SIGTERM
  tunnel daemon status  [--name X | --port N --mode http|tcp|udp] [--json]  Snapshot
  tunnel daemon monitor [--name X | --port N --mode http|tcp|udp]   Interactive TUI
  (tunnel stop / status / monitor remain registered as top-level aliases.)

Per-tunnel flags:
  --server string                 Relay server URL (overrides config)
  --token  string                 Auth token (overrides config)
  --name   string                 Request a stable tunnel name (e.g. 'myapp')
  --inspect                       Enable web inspector at 127.0.0.1:4040 (HTTP only).
                                  If 4040 is in use, inspector is silently skipped.

Global flags:
  -q, --quiet                     Suppress display output
      --debug                     Verbose debug logging
      --log-json                  Emit logs as JSON (slog-based)

Colors honour NO_COLOR, CLICOLOR=0, CLICOLOR_FORCE=1, and TERM=dumb.

Targeting daemons: when --name was used at start, address it with --name. When
no name was used, address it with --port + --mode (defaults to http).
```

### Example: `tunnel doctor`

```
config file: /home/you/.tunnel/config.yml
  ✓ permissions OK
server url:  https://relay.example.com
  ✓ scheme "https" OK
  ✓ host "relay.example.com"
relay reachability: GET https://relay.example.com/health
  ✓ relay responded 200 OK
auth token:  abcd***
  ✓ token present

If everything above is OK, try: tunnel http <port>
```

## Running Multiple Tunnels

Each tunnel is fully isolated — open as many as you need simultaneously. The URL form depends on whether the relay has `BASE_DOMAIN` set:

```sh
# subdomain mode (BASE_DOMAIN=relay.example.com on relay)
tunnel http 3000 --name api       # → https://api.relay.example.com/
tunnel http 5173 --name frontend  # → https://frontend.relay.example.com/
tunnel tcp 5432  --name db        # → db.relay.example.com:<dynamic-port>

# path-based mode (no BASE_DOMAIN)
tunnel http 3000 --name api       # → https://relay.example.com/t/api/
tunnel http 5173 --name frontend  # → https://relay.example.com/t/frontend/
```

Default per-token cap is 50 simultaneous tunnels (`TUNNEL_MAX_TUNNELS_PER_TOKEN`); per-IP cap 100. Hit the cap and the relay returns 429.

With subdomain routing each app gets its own browser origin, so cookies, WebSocket connections, and cached resources never mix between tunnels. See [Subdomain Routing](#subdomain-routing) below.

## Deploy the Relay Server

### Environment Variables

| Variable | Required | Description |
|---|---|---|
| `TUNNEL_AUTH_TOKENS` | Yes | Comma-separated list of valid auth tokens |
| `PORT` | No | HTTP server port (default `8080`) |
| `PUBLIC_HOST` | No | Hostname for TCP/UDP addresses sent to clients. Derived from HTTP `Host` header if unset |
| `BASE_DOMAIN` | No | Enables subdomain routing. Set to your relay domain (e.g. `tunnel.example.com`). Tunnels become `{id}.tunnel.example.com` |
| `CF_WORKER_ENABLED` | No | Set to `true` to enable Cloudflare Worker-based subdomain routing. When `false` (default), the relay uses direct DNS subdomain routing |
| `TUNNEL_WORKER_SECRET` | No | Shared secret for Cloudflare Worker integration. Only verified when `CF_WORKER_ENABLED=true` |
| `SINGLE_TUNNEL_MODE` | No | Set to `true` to allow only one tunnel at a time. Root domain (`tunnel.example.com`) proxies directly to it — no path prefix needed. Second tunnel connections are rejected. |
| `TUNNEL_RESPONSE_TIMEOUT` | No | Max time the relay waits for a CLI response before returning 504. Go duration syntax (e.g. `5m`, `120s`). Default `5m` |
| `TUNNEL_MAX_TUNNELS_PER_TOKEN` | No | Cap on simultaneous tunnels per auth token. Default `50`. Set to `0` to disable |
| `TUNNEL_MAX_TUNNELS_PER_IP` | No | Cap on simultaneous tunnels per remote IP. Default `100`. Set to `0` to disable |
| `TUNNEL_WS_ORIGINS` | No | Comma-separated Origin allowlist for `/ws/connect`. Empty (default) skips the check (CLI clients send no Origin header). Set only when expecting browser-only clients, e.g. `TUNNEL_WS_ORIGINS=https://my-app.com,https://other.com` |
| `TUNNEL_TRUST_PROXY` | No | Default `false` strips inbound `X-Forwarded-*` and rewrites from observed values. Set `true` ONLY when the binary sits behind a trusted TLS-terminating LB (Railway, Cloudflare, fly.io); the local app then sees the original client IP and `https` scheme. Setting `true` on the public edge would let any peer lie about their source IP |

### Railway

1. Fork this repo and connect it to [Railway](https://railway.app)
2. Railway auto-detects the Dockerfile
3. Set environment variables (see table above)
4. Generate a domain under Networking settings (e.g. `tunnel.example.com`)

> **Note:** TCP/UDP tunnels with per-tunnel dynamic ports work best on a self-hosted VPS where direct port access is available.

### Docker

```sh
docker build -t tunnel-server .
docker run -p 8080:8080 -e TUNNEL_AUTH_TOKENS=my-token tunnel-server
```

### From source

```sh
make build-server
TUNNEL_AUTH_TOKENS=my-token ./bin/tunnel-server
```

## Subdomain Routing

Subdomain routing gives each tunnel its own URL (`api.tunnel.example.com`) instead of a path prefix (`tunnel.example.com/t/api/`). This is the recommended mode when running multiple apps — each tunnel gets a separate browser origin so there is no cross-contamination between sessions.

There are two ways to enable it depending on your hosting plan.

### Option A — Direct wildcard DNS (VPS / Railway Pro)

If your server is reachable directly or you have a Railway plan that accepts a wildcard domain:

1. Add a `*.tunnel.example.com` DNS record pointing to your server
2. Set `BASE_DOMAIN=tunnel.example.com` on the relay

### Option B — Cloudflare Worker (Railway free tier)

Railway's free plan allows only one custom domain per service, so `*.tunnel.example.com` can't be added directly. A free Cloudflare Worker acts as the wildcard entry point and forwards requests to Railway.

**Architecture:**
```
Browser → *.tunnel.example.com → Cloudflare Worker → tunnel.example.com (Railway)
CLI     →                         tunnel.example.com (Railway, direct)
```

**Setup:**

1. Your domain's DNS must be managed by Cloudflare (add it as a zone — free)

2. Add a wildcard DNS record in Cloudflare:
   - Type: `CNAME`
   - Name: `*`
   - Target: `tunnel.example.com`
   - Proxy: enabled (orange cloud)

3. Create a Cloudflare Worker and paste the contents of `cloudflare-worker/index.js`

4. Add a Worker route: `*.tunnel.example.com/*` → your Worker

5. Set Worker environment variables:
   ```
   BASE_DOMAIN    = tunnel.example.com
   RAILWAY_URL    = https://tunnel.example.com
   WORKER_SECRET  = <generate with: openssl rand -hex 32>
   ```

6. Set Railway environment variables:
   ```
   BASE_DOMAIN            = tunnel.example.com
   TUNNEL_WORKER_SECRET   = <same value as WORKER_SECRET above>
   CF_WORKER_ENABLED      = true
   ```

That's it. Named tunnels now resolve to `{name}.tunnel.example.com`:

```sh
tunnel http 3000 --name api       # → https://api.tunnel.example.com
tunnel http 5173 --name frontend  # → https://frontend.tunnel.example.com
```

## Named Tunnels

Request a stable, human-readable ID instead of a random 16-char base62 one:

```sh
tunnel http 3000 --name eservice
```

**Name rules:** 1–32 characters, lowercase alphanumeric or hyphen, no leading/trailing hyphen.

**Reserved names** are rejected with `policy violation: invalid name` (close code 1008). The relay reserves server-control paths (`dashboard`, `metrics`, `health`, `ws`, `t`, `api`, `admin`) plus phishing-surface and infrastructure-style subdomains (`mail`, `login`, `auth`, `account`, `billing`, `secure`, `support`, `static`, `cdn`, `assets`, `app`, `docs`, `blog`, `status`, `staging`, `prod`, …). The full list lives in `internal/server/names.go`.

Names are first-come-first-served. When a named tunnel disconnects the name is held for 60 seconds so the same token can reconnect and reclaim it (matches the behavior for random IDs).

## Single Tunnel Mode

When you only need one tunnel and want to share a clean root URL with no path prefix or subdomain, set `SINGLE_TUNNEL_MODE=true` on the relay.

```sh
# Relay started with SINGLE_TUNNEL_MODE=true
tunnel http 3000 --name myapp
# → https://tunnel.example.com/  (root, no /t/myapp/ prefix)
```

**Behavior:**
- Root domain (`tunnel.example.com/`) proxies directly to the active tunnel
- If no tunnel is connected, visiting the root returns a "No tunnel connected" status page
- A second tunnel attempting to connect is rejected with an error: `single tunnel mode: a tunnel is already connected`

**Multi-tunnel mode (default):** Visiting the root domain with no active tunnel context shows a live list of all connected tunnels and their URLs.

## Daemon Mode

Release binaries and `make install` ship daemon support; only `go install` and `make install-no-daemon` skip it.

```sh
tunnel http 3000 --name api --silent
# tunnel daemonized (pid=12345, hash=abcd1234)
# logs:   /run/user/1000/tunnel/tunnel-abcd1234.log
# socket: /run/user/1000/tunnel/tunnel-abcd1234.sock
# monitor: tunnel monitor --name api

tunnel daemon status  --name api          # human-readable snapshot
tunnel daemon status  --name api --json   # machine-readable
tunnel daemon monitor --name api          # interactive TUI: requests, streams, traffic
tunnel daemon stop    --name api          # graceful SIGTERM
```

The parent process waits up to 3 seconds for the child to reach `state=connected` before exiting. If it can't (bad URL, bad token, network down) you see a one-line warning:

```
warning: daemon did not reach 'connected' state within 3s — check the log file or run `tunnel status`
```

**File layout** (per-user, mode 0700):
- Linux: `$XDG_RUNTIME_DIR/tunnel/` (falls back to `$TMPDIR/tunnel-<uid>/`)
- macOS: `$TMPDIR/tunnel-<uid>/`
- Windows: `%LOCALAPPDATA%\tunnel\` for PID/log; `\\.\pipe\tunnel-<hash>` for IPC

Each daemon owns three files there: `tunnel-<hash>.sock` (mode 0600), `.pid` (0600), `.log` (0600). Hash derives from `--name` or `mode + port`.

When no `--name` was used at start, target the daemon by `--port <N> --mode http|tcp|udp`. Pass `--log-file` to override the log path and `--max-log-size <bytes>` to enable size-based rotation (3-file ring).

## Relay Server Endpoints

| Endpoint | Auth | Description |
|---|---|---|
| `GET /health` | none | Liveness check (always 200 OK) |
| `GET /ws/connect` | Bearer token | WebSocket tunnel endpoint (used by the CLI) |
| `GET /t/{id}/...` | none | HTTP proxy to tunnel (path-based routing) |
| `{id}.{BASE_DOMAIN}/...` | none | HTTP proxy to tunnel (subdomain routing) |
| `GET /dashboard` | cookie / Bearer / `?token=` | Admin dashboard. First load with `?token=X` sets an HttpOnly cookie and redirects; subsequent loads require no token in URL |
| `GET /dashboard/api` | cookie / Bearer | JSON list of active tunnels (`tunnel list` calls this) |
| `GET /metrics` | Bearer / `?token=` | Prometheus metrics. Open when no `TUNNEL_AUTH_TOKENS` is configured |

## How It Works

### Connection setup

1. The CLI client opens a WebSocket to `relay/ws/connect`, authenticating via `Authorization: Bearer <token>`.
2. The relay validates the token (constant-time compare), checks per-token / per-IP caps, and assigns a tunnel ID (random 16-char base62, or the requested `--name`) — sent back as the first message.
3. The CLI prints the public URL and starts listening for messages from the relay.

### HTTP request flow

```
Browser GET https://tunnel.example.com/t/myapp/api/users
  → relay ProxyHandler
  → relay sends TypeHTTPRequest envelope to CLI client over WebSocket
  → CLI client makes GET http://localhost:3000/api/users
  → CLI sends TypeHTTPResponse back over WebSocket
  → relay writes response to browser
```

For large uploads the request body is streamed as `TypeHTTPRequestChunk` messages. For SSE and streaming responses the relay sends `TypeHTTPStreamChunk` messages until `TypeHTTPStreamEnd` or the browser disconnects.

### Subdomain routing with Cloudflare Worker

When `BASE_DOMAIN`, `TUNNEL_WORKER_SECRET`, and `CF_WORKER_ENABLED=true` are configured, the Cloudflare Worker handles `*.tunnel.example.com`:

1. Worker extracts the subdomain (e.g. `myapp` from `myapp.tunnel.example.com`)
2. Worker adds `X-Forwarded-Tunnel: myapp` and `X-Worker-Secret: <secret>` headers
3. Worker proxies the request to Railway (`tunnel.example.com`)
4. Relay's `subdomainMiddleware` verifies the secret with constant-time comparison and routes to tunnel `myapp`

The CLI connects directly to `tunnel.example.com` — it never goes through the Worker.

### SPA / Vite support (path-based routing)

When proxying HTML over path-based routing, the relay injects a small script into the `<head>` that:

- Rewrites `window.WebSocket`, `window.fetch`, `XMLHttpRequest.open`, and `window.EventSource` to route same-origin and localhost URLs through `/t/{id}/`
- Calls `history.replaceState` to strip the `/t/{id}/` prefix so SPA routers (React Router, Vue Router) see clean paths like `/dashboard` instead of `/t/myapp/dashboard`

With subdomain routing the script is not injected — the tunnel already has its own origin, so no URL rewriting is needed.

The CLI strips the `Origin` header before dialing the local WebSocket so dev servers that enforce same-origin checks (Vite 5.1+, Next.js Turbopack) do not reject the proxied upgrade.

### TCP/UDP

TCP and UDP tunnels get a dynamically allocated port on the relay. Connections to that port are relayed as multiplexed streams over the same WebSocket, each with a unique stream ID for concurrent handling.

### Protocol

Every message is two WebSocket frames:
1. A JSON text frame (the envelope — message type, request/stream ID, metadata)
2. A binary frame (the body — request/response body, chunk data, or empty)

A mutex-protected writer ensures both frames are written atomically, which is required for correct concurrent operation.

### Reconnection

If the WebSocket disconnects, the CLI reconnects with exponential backoff (1 s → 30 s max, ±25% jitter). It sends `?tunnel_id=<previous-id>` so the relay can restore the same public URL instead of assigning a new one. The relay holds the tunnel ID in a reservation for 60 seconds after disconnect.

## Configuration

Config is stored at `~/.tunnel/config.yml` (mode `0600`, parent dir `0700`):

```yaml
server_url: https://relay.example.com
auth_token: your-secret-token
```

`tunnel config show` prints the resolved config with the token masked. Permissions are tightened on every save, so an upgrade from a pre-1.7 binary that wrote `0644` is corrected the next time you run `tunnel config set-server` or `set-token`.

## Troubleshooting

- **`tunnel doctor`** is the first stop. It validates the config file, checks file permissions, parses the server URL, hits `/health`, and reports each step with a colored ✓/!/✗ marker.
- **Connection refused / DNS errors** are translated to one-line actionable messages instead of four-level error wrapping.
- **`tunnel http 3000` exits with `relay URL is invalid`** — the URL fails to parse. Re-run `tunnel config set-server <url>` (it now validates scheme and host).
- **429 Too Many Requests** on connect — per-token or per-IP cap reached. Bump `TUNNEL_MAX_TUNNELS_PER_TOKEN` / `TUNNEL_MAX_TUNNELS_PER_IP` or set them to `0`.
- **HTTP requests through the tunnel get `connection refused`** — local app is bound to `::1` only. Either bind it to `0.0.0.0` / `127.0.0.1`, or check the dev server's bind config. The CLI dials `127.0.0.1:<port>` explicitly.
- **`--inspect` shows "inspector disabled"** — port 4040 is in use; the tunnel runs without the inspector. Stop whatever owns 4040 (`lsof -i :4040`) or skip `--inspect`.
- **`--silent` warns about not reaching `connected`** — child daemon is running but cannot reach the relay. Check the log file (`logs:` line printed at start) or run `tunnel daemon status --name <X>`.
- **`--debug`** turns on verbose logging; **`--log-json`** routes the same logs through a structured JSON handler suitable for log aggregators.

## Known limitations

- Inspector port is hard-coded to `127.0.0.1:4040` (not configurable).
- Local apps must bind to `127.0.0.1` (or be dual-stack); apps that bind only to `::1` are unreachable.
- The relay does not terminate TLS itself — deploy behind a TLS-terminating load balancer (Railway, Cloudflare, fly.io) and set `TUNNEL_TRUST_PROXY=true` if upstream `X-Forwarded-*` headers should be honoured.
- Single dashboard URL per relay (no per-token namespacing).
- Daemon mode is Linux/macOS first-class; Windows daemon support uses named pipes and works but has fewer integration tests.

## Upgrading from v1.6.x

Operational changes worth knowing about (no API breaks for end users):

- **Daemon files moved.** `$TMPDIR/tunnel-<hash>.{sock,pid,log}` → `$XDG_RUNTIME_DIR/tunnel/tunnel-<hash>.{sock,pid,log}` (mode 0600 inside a 0700 dir). Stop any pre-1.7 daemon before upgrading; new `tunnel daemon stop|status|monitor` commands look only at the new path.
- **`TUNNEL_TRUST_PROXY` defaults to false.** If you deploy on Railway / Cloudflare / fly.io and want the original client IP and `https` scheme to reach the local app, set `TUNNEL_TRUST_PROXY=true` on the relay.
- **Per-token / per-IP caps default on.** 50 tunnels per token, 100 per IP. Heavy users can raise via `TUNNEL_MAX_TUNNELS_PER_TOKEN` / `TUNNEL_MAX_TUNNELS_PER_IP` or set to `0` to disable.
- **Tunnel ID length 8 → 16.** No API contract on length; bookmarked URLs that hard-coded an old random ID won't survive (named tunnels with `--name` are unaffected).
- **Config file mode.** Tightened to `0600` on every save; existing `0644` files are corrected on next `tunnel config set-...`.

## Development

```sh
make test               # run default-tag tests
make test-daemon        # run with -tags daemon
make test-all           # both
make build-server       # build relay binary → bin/tunnel-server
make build-client       # build CLI binary (no daemon)
make build-client-daemon# build CLI binary with daemon support
```

## Windows

`tunnel` works on Windows. ANSI color output is supported via Windows Terminal or PowerShell.

```sh
# Recommended: brokit (includes daemon mode)
brokit install tunnel

# Or download release binary directly:
# https://github.com/anivaryam/tunnel/releases

# Or via Go (no daemon mode):
go install github.com/anivaryam/tunnel/cmd/tunnel@latest
```

## License

MIT
