// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

// Package inflightlimit is a Caddy HTTP handler, inflight_limit
// (http.handlers.inflight_limit), that limits how many requests of one
// client are in flight at the same time, and optionally how many of one host
// altogether.
//
// Placed behind a cache and in front of reverse_proxy, it counts only what
// reaches the upstream: cache hits never get here. A client over its limit
// waits a short time for a slot, then gets 429; a host over its cap gets 503,
// which a cache in front can answer with a stale copy (stale-if-error).
//
// Why not a rate limit alone: a rate limit counts requests per window, hits
// and misses alike, and lets a burst of misses through as long as the window
// has room. Application workers (PHP-FPM, FrankenPHP, a fixed thread pool)
// are a concurrency resource; a burst of misses from one crawler queues in
// front of them and every other visitor waits behind it. Bounding
// concurrency per client bounds the queue one client can build.
package inflightlimit

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Defaults.
const (
	// DefaultPerClient is the Caddyfile's per_client when the block does not
	// name one.
	DefaultPerClient = 2
	// DefaultWait is how long a request over its client's limit waits for a
	// slot before it is refused.
	DefaultWait = time.Second
	// DefaultRetryAfter is the Retry-After of a refusal.
	DefaultRetryAfter = 5 * time.Second
)

// Rejection reasons, as logged and as the metric label.
const (
	ReasonPerClient = "per_client"
	ReasonPerHost   = "per_host"
)

func init() {
	caddy.RegisterModule(Handler{})
	// No directive order on purpose: the handler belongs right after the
	// cache handler, which has no fixed place either (`order cache before
	// reverse_proxy` in the Caddyfile). Use it inside a route, after `cache`.
	httpcaddyfile.RegisterHandlerDirective("inflight_limit", parseHandler)
}

// Handler limits concurrently in-flight requests per client and per host.
// A zero PerClient and a zero MaxPerHost make it a pass-through.
type Handler struct {
	// PerClient is the number of requests one client may have in flight
	// (0: no per-client limit). The client is its address masked with
	// IPv4Prefix / IPv6Prefix.
	PerClient int `json:"per_client,omitempty"`

	// Wait is how long a request over its client's limit waits for one of
	// the client's requests to finish before it gets 429 (default 1s,
	// negative: no wait).
	Wait caddy.Duration `json:"wait,omitempty"`

	// MaxPerHost is the number of requests in flight for one host (the
	// request's Host, without port) across all clients (0: no cap). A
	// request over it gets 503 at once.
	MaxPerHost int `json:"max_per_host,omitempty"`

	// RetryAfter is sent as Retry-After, in whole seconds, with 429 and 503
	// (default 5s).
	RetryAfter caddy.Duration `json:"retry_after,omitempty"`

	// IPv4Prefix and IPv6Prefix mask the client address to the network that
	// counts as one client (defaults 32 and 64).
	IPv4Prefix int `json:"ipv4_prefix,omitempty"`
	IPv6Prefix int `json:"ipv6_prefix,omitempty"`

	state   *state
	poolKey string
	logger  *zap.Logger
	metrics *metrics
}

// state is what outlives a config reload: the slots held by requests that
// are still running. Shared between handler instances of the same settings
// through a usage pool, so a reload does not start every client at zero while
// its requests still run.
type state struct {
	clients *slots
	hosts   *slots
}

func (*state) Destruct() error { return nil }

var states = caddy.NewUsagePool()

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.inflight_limit",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision applies defaults and attaches the shared state.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	if h.Wait == 0 {
		h.Wait = caddy.Duration(DefaultWait)
	}
	if h.RetryAfter == 0 {
		h.RetryAfter = caddy.Duration(DefaultRetryAfter)
	}
	if h.IPv4Prefix == 0 {
		h.IPv4Prefix = DefaultIPv4Prefix
	}
	if h.IPv6Prefix == 0 {
		h.IPv6Prefix = DefaultIPv6Prefix
	}
	if !h.enabled() {
		return nil
	}

	perClient, perHost := h.PerClient, h.MaxPerHost
	if perClient <= 0 {
		perClient = math.MaxInt
	}
	if perHost <= 0 {
		perHost = math.MaxInt
	}
	h.poolKey = fmt.Sprintf("inflight_limit/%d/%d/%d/%d", h.PerClient, h.MaxPerHost, h.IPv4Prefix, h.IPv6Prefix)
	v, _, err := states.LoadOrNew(h.poolKey, func() (caddy.Destructor, error) {
		return &state{clients: newSlots(perClient), hosts: newSlots(perHost)}, nil
	})
	if err != nil {
		return err
	}
	h.state = v.(*state)

	m, err := registerMetrics(ctx.GetMetricsRegistry())
	if err != nil {
		return err
	}
	h.metrics = m
	return nil
}

// Validate checks the settings.
func (h *Handler) Validate() error {
	if h.PerClient < 0 {
		return fmt.Errorf("per_client must not be negative, got %d", h.PerClient)
	}
	if h.MaxPerHost < 0 {
		return fmt.Errorf("max_per_host must not be negative, got %d", h.MaxPerHost)
	}
	if h.RetryAfter < 0 {
		return fmt.Errorf("retry_after must not be negative")
	}
	return validatePrefixes(h.IPv4Prefix, h.IPv6Prefix)
}

// Cleanup releases the shared state.
func (h *Handler) Cleanup() error {
	if h.poolKey == "" {
		return nil
	}
	_, err := states.Delete(h.poolKey)
	return err
}

func (h *Handler) enabled() bool { return h.PerClient > 0 || h.MaxPerHost > 0 }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.state == nil {
		return next.ServeHTTP(w, r)
	}
	client := clientKey(r, h.IPv4Prefix, h.IPv6Prefix)
	host := hostOf(r)

	if h.PerClient > 0 {
		start := time.Now()
		if !h.state.clients.acquire(r.Context(), client, time.Duration(h.Wait)) {
			if r.Context().Err() != nil {
				// The client went away while it waited; nobody to answer.
				return r.Context().Err()
			}
			held, waiting := h.state.clients.inFlight(client)
			h.reject(w, r, http.StatusTooManyRequests, ReasonPerClient, client, host, held, waiting, time.Since(start))
			return nil
		}
		if waited := time.Since(start); waited > time.Millisecond {
			h.metrics.waited.WithLabelValues(host).Inc()
		}
		defer h.state.clients.release(client)
	}

	if !h.state.hosts.acquire(r.Context(), host, 0) {
		held, _ := h.state.hosts.inFlight(host)
		h.reject(w, r, http.StatusServiceUnavailable, ReasonPerHost, client, host, held, 0, 0)
		return nil
	}
	h.metrics.inFlight.WithLabelValues(host).Inc()
	defer func() {
		h.metrics.inFlight.WithLabelValues(host).Dec()
		h.state.hosts.release(host)
	}()

	return next.ServeHTTP(w, r)
}

// reject answers the request itself instead of returning an error. An error
// would go to handle_errors, which may mask the status, and lose the
// headers; and a cache in front (Souin) treats a written 503 from upstream
// as an upstream failure, which lets it serve a stale copy under
// stale-if-error. no-store keeps the refusal out of every cache: Souin would
// otherwise give a response without Cache-Control its default policy.
func (h *Handler) reject(w http.ResponseWriter, r *http.Request, status int, reason, client, host string, held, waiting int, waited time.Duration) {
	h.metrics.rejected.WithLabelValues(host, reason).Inc()
	h.logger.Info("request refused",
		zap.String("reason", reason),
		zap.String("client", client),
		zap.String("host", host),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("user_agent", r.UserAgent()),
		zap.Int("status", status),
		zap.Int("in_flight", held),
		zap.Int("waiting", waiting),
		zap.Duration("waited", waited),
	)
	hdr := w.Header()
	hdr.Set("Retry-After", strconv.Itoa(int(math.Ceil(time.Duration(h.RetryAfter).Seconds()))))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "%d %s\n", status, http.StatusText(status))
}

func hostOf(r *http.Request) string {
	host := r.Host
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// metrics are registered once per config (Caddy hands every config load a
// fresh registry); a second handler instance in the same config reuses them.
type metrics struct {
	inFlight *prometheus.GaugeVec
	rejected *prometheus.CounterVec
	waited   *prometheus.CounterVec
}

func registerMetrics(reg *prometheus.Registry) (*metrics, error) {
	m := &metrics{
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "caddy", Subsystem: "http_inflight_limit", Name: "in_flight",
			Help: "Requests past inflight_limit that have not finished, per host.",
		}, []string{"host"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "caddy", Subsystem: "http_inflight_limit", Name: "rejected_total",
			Help: "Requests refused by inflight_limit, per host and reason (per_client: 429, per_host: 503).",
		}, []string{"host", "reason"}),
		waited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "caddy", Subsystem: "http_inflight_limit", Name: "waited_total",
			Help: "Requests that waited for a slot of their client and then got one, per host.",
		}, []string{"host"}),
	}
	if reg == nil {
		return m, nil
	}
	var err error
	if m.inFlight, err = register(reg, m.inFlight); err != nil {
		return nil, err
	}
	if m.rejected, err = register(reg, m.rejected); err != nil {
		return nil, err
	}
	if m.waited, err = register(reg, m.waited); err != nil {
		return nil, err
	}
	return m, nil
}

func register[C prometheus.Collector](reg *prometheus.Registry, c C) (C, error) {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(C); ok {
				return existing, nil
			}
		}
		return c, err
	}
	return c, nil
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens:
//
//	inflight_limit [<matcher>] {
//	    per_client   <n>          # default 2; 0 switches the per-client limit off
//	    wait         <duration>   # default 1s; 0 refuses at once
//	    max_per_host <n>          # default 0 (no cap)
//	    retry_after  <duration>   # default 5s; a bare number is seconds
//	    ipv4_prefix  <bits>       # default 32
//	    ipv6_prefix  <bits>       # default 64
//	}
//
// per_client 0 together with max_per_host 0 (or unset) is a pass-through, so
// an environment placeholder can switch the handler off per instance.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	h.PerClient = DefaultPerClient
	for d.NextBlock(0) {
		switch d.Val() {
		case "per_client":
			n, err := intArg(d)
			if err != nil {
				return err
			}
			h.PerClient = n
		case "max_per_host":
			n, err := intArg(d)
			if err != nil {
				return err
			}
			h.MaxPerHost = n
		case "wait":
			dur, err := durationArg(d)
			if err != nil {
				return err
			}
			if dur == 0 {
				dur = -1 // explicit: no waiting
			}
			h.Wait = caddy.Duration(dur)
		case "retry_after":
			dur, err := durationArg(d)
			if err != nil {
				return err
			}
			h.RetryAfter = caddy.Duration(dur)
		case "ipv4_prefix":
			n, err := intArg(d)
			if err != nil {
				return err
			}
			h.IPv4Prefix = n
		case "ipv6_prefix":
			n, err := intArg(d)
			if err != nil {
				return err
			}
			h.IPv6Prefix = n
		default:
			return d.Errf("unknown inflight_limit option '%s'", d.Val())
		}
	}
	return nil
}

func durationArg(d *caddyfile.Dispenser) (time.Duration, error) {
	name := d.Val()
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	val := d.Val()
	if d.NextArg() {
		return 0, d.ArgErr()
	}
	if n, err := strconv.Atoi(val); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	dur, err := caddy.ParseDuration(val)
	if err != nil {
		return 0, d.Errf("%s: %v", name, err)
	}
	if dur < 0 {
		return 0, d.Errf("%s must not be negative", name)
	}
	return dur, nil
}

func parseHandler(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(Handler)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
