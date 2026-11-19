<!--
SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
SPDX-License-Identifier: Apache-2.0
-->

# caddy-inflight-limit

A Caddy HTTP handler, `inflight_limit`, that limits how many requests of one
client are **in flight at the same time**, and optionally how many of one
host altogether. Built to sit between a cache and `reverse_proxy`, so that it
counts only the requests that actually reach the application.

## Status

Version 0.x: the directive and its options may still change before 1.0. The
module has been used in production behind a cache since November 2026.

## Why

A typical PHP site behind Caddy: a cache (for example Souin) answers most
requests, and the rest go to a small, fixed pool of application workers
(PHP-FPM children, FrankenPHP workers). The workers are a **concurrency**
resource: two workers render two pages at a time, and every further request
waits.

A rate limit does not bound that. It counts requests per window, cache hits
and misses alike. A crawler that sends a few hundred requests in a few
seconds stays inside a window that ordinary visitors also need, and the
misses among them — often a hundred or more for a crawler walking the long
tail — arrive at the workers at once. They queue, and every other visitor
whose request is a miss in those seconds waits behind them.

`inflight_limit` bounds what one client can have in flight. A burst then costs
the crawler waiting and 429s, not other visitors' time. Cache hits are never
counted, so a visitor paging quickly through cached content is not affected.

A client is its address masked to a network: by default the IPv4 address and
the IPv6 **/64**, because one host can send from any address of its /64.

## Behaviour

- A request over its client's limit **waits** up to `wait` for one of the
  client's requests to finish. Slots are handed over in arrival order; a
  newcomer never overtakes a waiting request. If no slot frees up in time, the
  request gets **429 Too Many Requests**.
- A request over the host's cap gets **503 Service Unavailable** at once.
- Both answers carry `Retry-After` and `Cache-Control: no-store`, and are
  written by the handler (not returned as errors), so `handle_errors` does not
  turn them into something else.
- Each refusal is logged once at INFO by `http.handlers.inflight_limit`
  (`"request refused"`, with `reason`, masked `client`, `host`, `method`,
  `path`, `user_agent`, `status`, `in_flight`, `waiting`, `waited`).
- A client that goes away while waiting is not answered and not logged.
- The slots outlive a config reload: requests still running under the old
  config keep counting.
- `per_client 0` and no host cap make the handler a pass-through, so the
  limit can be switched per instance through an environment placeholder.

### Stale content instead of 503

A cache in front treats the handler's 503 as an upstream failure. With Souin,
an expired entry is served instead **if the stored response allows
`stale-if-error`** and the entry is still within Souin's `stale` window.
Measured with Caddy 2.11.9 and Souin 1.7.10: an expired page whose stored
`Cache-Control` carried `stale-if-error=3600` was answered with the stored
copy (`Cache-Status: …; fwd=stale; fwd-status=503`); without it, the 503
reached the client.

Many applications send no `stale-if-error`. A `header` directive between the
cache and the limiter adds it to what the cache stores (see the example
below). It then also applies to every other upstream 5xx.

## Installation

```sh
xcaddy build --with github.com/ohartwig/caddy-inflight-limit
```

## Caddyfile

```caddyfile
inflight_limit [<matcher>] {
    per_client   <n>          # requests in flight per client; default 2, 0 = no per-client limit
    wait         <duration>   # how long a request over the limit waits; default 1s, 0 = refuse at once
    max_per_host <n>          # requests in flight per host, all clients; default 0 = no cap
    retry_after  <duration>   # Retry-After of 429 and 503; default 5s, a bare number is seconds
    ipv4_prefix  <bits>       # IPv4 network that counts as one client; default 32
    ipv6_prefix  <bits>       # IPv6 network that counts as one client; default 64
}
```

The directive has no fixed place in Caddy's directive order, because the
cache it belongs behind has none either. Use it inside a `route`, after the
cache handler.

### Behind Souin

```caddyfile
{
    order cache before reverse_proxy
    cache {
        stale 60m
    }
}

example.com {
    route {
        cache

        # Let the cache serve an expired copy when the limiter (or the
        # application) answers 5xx. Below the cache, so it is stored.
        header {
            Cache-Control "^(.*)$" "$1, stale-if-error=3600"
            defer
        }

        # Static files do not occupy a worker; do not count them.
        @dynamic not path *.css *.js *.png *.jpg *.svg *.woff2 /assets/*
        inflight_limit @dynamic {
            per_client 2
            wait 1s
            max_per_host 16
        }

        reverse_proxy app:9000
    }
}
```

### Switched per instance

```caddyfile
inflight_limit {
    per_client   {$INFLIGHT_PER_CLIENT:0}
    max_per_host {$INFLIGHT_MAX_PER_HOST:0}
}
```

Both unset: the handler does nothing.

### Behind a proxy

The client is Caddy's client IP. Behind a load balancer or another proxy,
configure `trusted_proxies` and `client_ip_headers` in the server options, or
every request counts as the proxy's:

```caddyfile
{
    servers {
        trusted_proxies static 10.0.0.0/8
        client_ip_headers X-Forwarded-For
    }
}
```

### Grouping a rate limit by /64 as well

`inflight_limit` does not replace a rate limit; it bounds concurrency, a rate
limit bounds volume. To count the same networks in
[caddy-ratelimit](https://github.com/mholt/caddy-ratelimit), use its own
`ipv6_prefix`:

```caddyfile
rate_limit {
    zone per_client {
        key {client_ip}
        ipv6_prefix 64
        events 150
        window 10s
    }
}
```

## JSON

```json
{
    "handler": "inflight_limit",
    "per_client": 2,
    "wait": "1s",
    "max_per_host": 16,
    "retry_after": "5s",
    "ipv4_prefix": 32,
    "ipv6_prefix": 64
}
```

In JSON, an omitted `per_client` is 0 (no per-client limit); the Caddyfile
default of 2 applies only to the Caddyfile. Durations are Caddy durations; a
negative `wait` refuses at once.

## Metrics

Registered in Caddy's metrics registry, per host:

| Metric | Meaning |
|---|---|
| `caddy_http_inflight_limit_in_flight` | requests past the limiter that have not finished |
| `caddy_http_inflight_limit_rejected_total{reason}` | refusals, `per_client` (429) or `per_host` (503) |
| `caddy_http_inflight_limit_waited_total` | requests that waited for a slot and got one |

The `host` label is the request's Host header; put the handler behind
something that only routes known hosts, or the label set grows with what
clients send.

## Caveats

- **Shared networks share slots.** Clients behind one IPv4 address (a
  carrier-grade NAT, an office) or one /64 are one client. The wait absorbs
  ordinary parallelism; check the refusal log before tightening the limit.
- **Request coalescing.** A cache that coalesces concurrent misses of the same
  URL (Souin does) gives the waiting requests the answer of the one that went
  upstream — a refusal included.
- **One instance, one count.** The counters are per Caddy process. Several
  instances behind a load balancer each allow the limit.

## Measurements

<!-- TODO: measured numbers are added before the first public release
     (queuing time of cache misses with and without the limiter, test
     setup, versions). Do not publish figures that are not reproducible. -->

To be added.

## Development

```sh
go test -race ./...
```

The tests cover the slot accounting (limit never exceeded under concurrency,
arrival order, no overtaking, timeout, cancellation, cleanup), the masking,
wait-then-429, the host cap, pass-through, reloads, Caddyfile parsing, and an
integration test that runs Caddy from a Caddyfile on loopback.

## Security

See [SECURITY.md](SECURITY.md).

## Licence

Apache-2.0, see [LICENSE](LICENSE).
