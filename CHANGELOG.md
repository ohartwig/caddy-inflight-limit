# Changelog

## 0.2.0 (2026-11-25)

First public release.

Initial public release of `inflight_limit`, a Caddy HTTP handler that limits
the requests of one client that are in flight at the same time, and optionally
those of one host.

- Per-client limit with a bounded, first-come-first-served wait; refusals
  answer 429 (client) or 503 (host) with `Retry-After` and `no-store`.
- Clients are grouped by network prefix (default IPv4 /32, IPv6 /64).
- Prometheus metrics in Caddy's registry; one INFO log line per refusal.
- Caddyfile and JSON configuration.
