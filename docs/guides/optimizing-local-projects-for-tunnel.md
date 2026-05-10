# Optimizing Local Projects for tunnel

This guide helps you configure a local project so it works reliably when exposed through `tunnel`.

Use this when you run something like:

```sh
tunnel http 3000 --name myapp
```

and want the public URL to behave like a normal production URL.

## Quick Checklist

- Prefer relative URLs in your app: `/api/users`, not `http://localhost:3000/api/users`.
- Use `--name` for stable URLs, especially when your relay has `BASE_DOMAIN` enabled.
- Make sure your local server is actually reachable at the port you pass to `tunnel`.
- Configure CORS to allow the tunnel URL, not only `localhost`.
- Configure cookies for HTTPS and the public tunnel host when testing auth.
- Configure OAuth/webhook callback URLs to use the public tunnel URL.
- For Vite/WebSocket/HMR issues, make sure the dev server accepts the tunnel host.

## How tunnel Works Conceptually

`tunnel` connects a public relay server to your local machine over one outbound WebSocket connection.

```text
Browser -> Relay Server -> WebSocket -> tunnel CLI -> localhost:<port>
```

The browser never connects directly to your laptop. It connects to the relay URL. The relay forwards each request through the `tunnel` CLI, and the CLI forwards it to your local server.

That means your local app should behave as if it is behind a reverse proxy:

- The public scheme is usually `https`.
- The public host is the tunnel host, not `localhost`.
- Headers like `Host`, `X-Forwarded-Host`, `X-Forwarded-Proto`, and `X-Forwarded-For` matter.
- Hardcoded localhost URLs may bypass the tunnel and fail for remote users.

## Best Local Server Settings

Run your local app normally, then point `tunnel` at the same port:

```sh
npm run dev
tunnel http 5173 --name frontend
```

Recommended habits:

- Use a stable tunnel name for projects you open often: `--name frontend`, `--name api`, `--name demo`.
- Keep one local app per tunnel when possible.
- Use your app's normal development port, not the relay port.
- Restart the tunnel if your local server changes ports.
- If the app generates external links, configure its public base URL to the tunnel URL.

If your relay uses `BASE_DOMAIN`, named tunnels become subdomains:

```text
https://frontend.relay.example.com/
```

If the relay does not use `BASE_DOMAIN`, tunnels use path-based routing:

```text
https://relay.example.com/t/frontend/
```

## Binding to localhost vs 0.0.0.0

For most HTTP projects, binding your app to `localhost` or `127.0.0.1` is enough:

```sh
tunnel http 3000
```

The `tunnel` CLI runs on the same machine and connects to `127.0.0.1:<port>`.

Use `0.0.0.0` only when your framework requires it or when the app is running inside a container/VM and the tunnel client is outside that container.

Examples:

```sh
# Local app on the same host as tunnel: localhost is fine
python -m http.server 8000
tunnel http 8000

# App inside Docker, tunnel on host: publish the container port first
```

Avoid exposing your dev server directly on your LAN unless you need it. `tunnel` only needs local access.

## Handling URLs, CORS, Cookies, WebSockets, and SSE

### Absolute URLs

Prefer relative URLs in browser code:

```js
fetch('/api/users')
```

Avoid hardcoded localhost URLs:

```js
fetch('http://localhost:3000/api/users')
```

Hardcoded localhost means “the user's machine”, not your machine. It will fail for anyone else opening the tunnel URL.

If your framework needs a public app URL, set it to the tunnel URL:

```env
APP_URL=https://myapp.relay.example.com
NEXT_PUBLIC_APP_URL=https://myapp.relay.example.com
PUBLIC_BASE_URL=https://myapp.relay.example.com
```

### Asset URLs

Use root-relative or relative asset paths:

```html
<script src="/assets/app.js"></script>
<link rel="stylesheet" href="/assets/app.css">
```

Avoid hardcoded dev-origin assets unless your app deliberately serves assets from another server.

If you use path-based routing (`/t/<id>/...`), `tunnel` injects a rewrite helper into HTML so most SPA asset and API calls continue to work. Subdomain routing is cleaner because the app gets its own origin.

### CORS

If your frontend and API are exposed through different tunnel URLs, configure the API to allow the frontend's public tunnel origin:

```text
Access-Control-Allow-Origin: https://frontend.relay.example.com
```

Do not configure only `http://localhost:5173` if the browser is visiting `https://frontend.relay.example.com`.

For cookie-based auth across origins, also allow credentials:

```text
Access-Control-Allow-Credentials: true
```

And do not use `Access-Control-Allow-Origin: *` with credentials.

### Cookies

When testing auth through a tunnel, remember the browser sees the tunnel URL, not localhost.

Common settings:

- Use `Secure` cookies because the public tunnel URL is HTTPS.
- Use `SameSite=Lax` for normal app navigation.
- Use `SameSite=None; Secure` for cross-site iframe or third-party flows.
- Set cookie domain only if you really need sharing across subdomains.

For most local tests, host-only cookies are simplest. Do not set `Domain=localhost` when using a public tunnel host.

### WebSockets

Use relative WebSocket URLs when possible:

```js
const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
const ws = new WebSocket(`${protocol}//${location.host}/ws`)
```

Avoid hardcoded localhost WebSocket URLs:

```js
new WebSocket('ws://localhost:3000/ws')
```

For path-based routing, `tunnel` tries to rewrite common browser WebSocket calls in HTML responses. Subdomain routing is still better for WebSocket-heavy apps because the URL path does not need a tunnel prefix.

### Server-Sent Events (SSE)

SSE works best when your local server sends the correct headers and flushes events:

```text
Content-Type: text/event-stream
Cache-Control: no-cache
```

If the stream appears to hang, check that your framework is flushing chunks and not buffering the whole response.

## Recommended Config for Common Stacks

### Vite / React

Use a named tunnel:

```sh
npm run dev -- --host 127.0.0.1
tunnel http 5173 --name frontend
```

Recommendations:

- Use relative API calls: `/api`, not `http://localhost:...`.
- If Vite rejects the tunnel host, add it to `server.allowedHosts`.
- If HMR has issues, configure HMR host/protocol for the tunnel URL.

Example `vite.config.ts`:

```ts
export default {
  server: {
    allowedHosts: ['frontend.relay.example.com'],
    hmr: {
      protocol: 'wss',
      host: 'frontend.relay.example.com',
    },
  },
}
```

If HMR is not important for remote viewers, you can ignore HMR errors as long as normal page loads work.

### Next.js

Run Next normally and expose its port:

```sh
npm run dev
tunnel http 3000 --name nextapp
```

Recommendations:

- Set public environment variables to the tunnel URL if your app builds absolute URLs.
- Add the tunnel host to image/domain allowlists if using `next/image` with remote URLs.
- Configure OAuth callback URLs to the tunnel URL.

Example:

```env
NEXT_PUBLIC_APP_URL=https://nextapp.relay.example.com
AUTH_URL=https://nextapp.relay.example.com
```

### Django

Run Django locally:

```sh
python manage.py runserver 127.0.0.1:8000
tunnel http 8000 --name django
```

Recommended settings for tunnel testing:

```py
ALLOWED_HOSTS = ['django.relay.example.com', 'localhost', '127.0.0.1']
CSRF_TRUSTED_ORIGINS = ['https://django.relay.example.com']
SECURE_PROXY_SSL_HEADER = ('HTTP_X_FORWARDED_PROTO', 'https')
```

For cookies over HTTPS:

```py
SESSION_COOKIE_SECURE = True
CSRF_COOKIE_SECURE = True
```

### Flask

Run Flask locally:

```sh
flask run --host 127.0.0.1 --port 5000
tunnel http 5000 --name flask
```

Recommendations:

- Use `ProxyFix` if your app needs correct scheme/host from proxy headers.
- Set callback URLs and generated absolute URLs to the tunnel URL.

Example:

```py
from werkzeug.middleware.proxy_fix import ProxyFix
app.wsgi_app = ProxyFix(app.wsgi_app, x_for=1, x_proto=1, x_host=1)
```

### FastAPI

Run FastAPI locally:

```sh
uvicorn app:app --host 127.0.0.1 --port 8000
tunnel http 8000 --name fastapi
```

Recommendations:

- Add CORS origins for your tunnel frontend URL.
- If generating absolute URLs, respect `X-Forwarded-Proto` and `X-Forwarded-Host`.

Example CORS setup:

```py
app.add_middleware(
    CORSMiddleware,
    allow_origins=['https://frontend.relay.example.com'],
    allow_credentials=True,
    allow_methods=['*'],
    allow_headers=['*'],
)
```

### Rails

Run Rails locally:

```sh
bin/rails server -b 127.0.0.1 -p 3000
tunnel http 3000 --name rails
```

Recommendations:

- Add the tunnel host to `config.hosts` in development.
- Configure `default_url_options` if the app generates absolute URLs.
- Use the tunnel URL for OAuth callbacks and webhooks.

Example:

```rb
config.hosts << "rails.relay.example.com"
```

### Laravel

Run Laravel locally:

```sh
php artisan serve --host=127.0.0.1 --port=8000
tunnel http 8000 --name laravel
```

Recommended `.env` values:

```env
APP_URL=https://laravel.relay.example.com
SESSION_SECURE_COOKIE=true
```

If using Sanctum or cross-origin SPA auth, also configure the public tunnel host in the relevant stateful domain / CORS settings.

### Static Servers

For a static directory:

```sh
python -m http.server 8000
tunnel http 8000 --name static
```

Recommendations:

- Use relative links when possible.
- Avoid absolute `http://localhost` references in HTML.
- If deploying a built SPA under path-based routing, prefer subdomain routing to avoid base path issues.

## Base-Domain and Subdomain Routing Notes

When the relay has `BASE_DOMAIN` configured, each tunnel becomes a subdomain:

```sh
tunnel http 3000 --name api
# https://api.relay.example.com/
```

This is the best mode for most web apps because each tunnel gets a clean browser origin.

Requirements:

- DNS must route `*.relay.example.com` to your relay.
- If using Cloudflare or another proxy, make sure it forwards WebSocket upgrades.
- Use lowercase names: `--name myapp`, not `--name MyApp`.
- Use `--name` when you need stable OAuth, webhook, or team-share URLs.

Unnamed tunnels also work, but their URL changes each time:

```sh
tunnel http 3000
# https://k9x2m4q8p1z0abcd.relay.example.com/
```

If you see `tunnel not found` with an unnamed subdomain, update both the relay server and client to the latest release.

## Troubleshooting

### “No tunnel found” or “tunnel not found”

Check:

- Is the tunnel still running?
- Are you visiting the exact URL printed by the CLI?
- If using `BASE_DOMAIN`, does wildcard DNS point to the relay?
- If using Cloudflare Worker routing, is `X-Forwarded-Tunnel` being set correctly?
- If using an old version, update to the latest release for lowercase unnamed subdomains.

For stable URLs, prefer:

```sh
tunnel http 3000 --name myapp
```

### Assets return 404

Common causes:

- The app generated asset URLs for `localhost`.
- The app assumes it is hosted at `/`, but you are using path-based `/t/<id>/` routing.
- A built SPA has a fixed base path.

Fixes:

- Use subdomain routing when possible.
- Use relative asset paths.
- Configure your bundler base URL correctly.
- For Vite, avoid setting `base` to a localhost URL.

### CORS errors

The browser origin is the tunnel URL. Add that URL to your API's CORS allowlist.

If frontend and API use separate tunnels:

```text
Frontend: https://frontend.relay.example.com
API:      https://api.relay.example.com
```

Then the API must allow `https://frontend.relay.example.com`.

### WebSocket / HMR not connecting

Check:

- Does your framework allow the tunnel host?
- Is the WebSocket URL hardcoded to `localhost`?
- Does your relay/proxy support WebSocket upgrades?
- Are you using `wss://` from an HTTPS tunnel page?

For remote demos, HMR is often optional. If the page works but HMR fails, the app may still be fine for sharing.

### Redirects go to localhost

Your app is generating absolute redirects using its local idea of the host.

Fixes:

- Set the app public URL to the tunnel URL.
- Make the framework trust proxy headers.
- Use `X-Forwarded-Host` and `X-Forwarded-Proto` when generating redirects.

Examples:

```env
APP_URL=https://myapp.relay.example.com
PUBLIC_BASE_URL=https://myapp.relay.example.com
```

### Cookies do not stick

Check:

- Is the cookie being set for `localhost` instead of the tunnel host?
- Is `Secure` missing on an HTTPS page?
- Is `SameSite` too strict for your auth flow?
- Are frontend and API on different origins?

For simple same-origin testing through one tunnel, host-only cookies usually work best.

## Security Checklist

- Use relay auth tokens; do not run a public relay with no auth unless you intend to.
- Do not expose admin panels, databases, or internal tools unless you understand the risk.
- Prefer `--name` values that do not reveal sensitive project names for public demos.
- Stop tunnels when you are done: press `Ctrl+C` or use `tunnel daemon stop`.
- Treat the public tunnel URL as internet-facing.
- Do not paste secrets into URLs; URLs can appear in logs and browser history.
- Keep your relay and client updated.
