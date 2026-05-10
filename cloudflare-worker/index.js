export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    // Strip port before suffix check (e.g. "myapp.tunnel.example.com:443" → "myapp.tunnel.example.com")
    const host = (request.headers.get("host") ?? url.hostname).split(":")[0];
    const suffix = "." + env.BASE_DOMAIN;

    // Reject if host doesn't end with the expected base domain suffix.
    if (!host.endsWith(suffix)) {
      return new Response("not found", { status: 404 });
    }

    // Extract single-level subdomain: "myapp.tunnel.example.com" → "myapp"
    const tunnelID = host.slice(0, host.length - suffix.length);

    // Reject if no subdomain or multi-level (foo.bar.tunnel.example.com)
    if (!tunnelID || tunnelID.includes(".")) {
      return new Response("not found", { status: 404 });
    }

    const target = new URL(url.pathname + url.search, env.RAILWAY_URL);
    const headers = new Headers(request.headers);
    headers.set("X-Forwarded-Tunnel", tunnelID);
    headers.set("X-Worker-Secret", env.WORKER_SECRET);

    return fetch(new Request(target.toString(), {
      method: request.method,
      headers,
      body: request.body,
    }));
  },
};