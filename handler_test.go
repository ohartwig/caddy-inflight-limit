// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

package inflightlimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestMaskAddr(t *testing.T) {
	cases := []struct {
		in     string
		v4, v6 int
		want   string
	}{
		{"203.0.113.7", 32, 64, "203.0.113.7"},
		{"203.0.113.7", 24, 64, "203.0.113.0/24"},
		{"2001:db8:1:2:3:4:5:6", 32, 64, "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", 32, 64, "2001:db8:1:2::/64"},
		{"2001:db8:1:2:3:4:5:6", 32, 128, "2001:db8:1:2:3:4:5:6"},
		{"2001:db8:1:2:3:4:5:6", 32, 56, "2001:db8:1::/56"},
		{"::ffff:203.0.113.7", 32, 64, "203.0.113.7"},
		{"fe80::1%eth0", 32, 64, "fe80::/64"},
		{"not-an-ip", 32, 64, "not-an-ip"},
	}
	for _, c := range cases {
		if got := maskAddr(c.in, c.v4, c.v6); got != c.want {
			t.Errorf("maskAddr(%q, %d, %d) = %q, want %q", c.in, c.v4, c.v6, got, c.want)
		}
	}
}

// request builds a request as Caddy's server prepares it, with the given
// client IP in the vars.
func request(clientIP, host string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/page", nil)
	ctx := context.WithValue(r.Context(), caddyhttp.VarsCtxKey, map[string]any{
		caddyhttp.ClientIPVarKey: clientIP,
	})
	return r.WithContext(ctx)
}

// blockingUpstream holds every request until release is closed.
type blockingUpstream struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingUpstream() *blockingUpstream {
	return &blockingUpstream{entered: make(chan struct{}, 100), release: make(chan struct{})}
}

func (b *blockingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	b.entered <- struct{}{}
	<-b.release
	w.WriteHeader(http.StatusOK)
	return nil
}

func provisioned(t *testing.T, h *Handler) (*Handler, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	h.logger = zap.New(core)
	t.Cleanup(func() { _ = h.Cleanup() })
	return h, logs
}

func serve(h *Handler, r *http.Request, next caddyhttp.Handler) (*httptest.ResponseRecorder, chan error) {
	rec := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() { done <- h.ServeHTTP(rec, r, next) }()
	return rec, done
}

func TestHandlerWaitThen429(t *testing.T) {
	h, logs := provisioned(t, &Handler{PerClient: 2, Wait: caddy.Duration(80 * time.Millisecond), RetryAfter: caddy.Duration(5 * time.Second)})
	up := newBlockingUpstream()

	// Two requests of one /64, from different addresses in it, take both slots.
	_, d1 := serve(h, request("2001:db8:1:2::1", "example.org"), up)
	_, d2 := serve(h, request("2001:db8:1:2::2", "example.org"), up)
	<-up.entered
	<-up.entered

	// A third of the same /64 waits, then is refused.
	start := time.Now()
	rec := httptest.NewRecorder()
	if err := h.ServeHTTP(rec, request("2001:db8:1:2:aaaa::3", "example.org"), up); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: %d, want 429", rec.Code)
	}
	if waited := time.Since(start); waited < 80*time.Millisecond {
		t.Fatalf("refused after %v, before the wait was over", waited)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control %q", got)
	}

	// Another /64 is not affected.
	_, d4 := serve(h, request("2001:db8:1:3::1", "example.org"), up)
	select {
	case <-up.entered:
	case <-time.After(time.Second):
		t.Fatal("a different /64 was held back")
	}

	close(up.release)
	for _, d := range []chan error{d1, d2, d4} {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}

	entries := logs.FilterMessage("request refused").All()
	if len(entries) != 1 {
		t.Fatalf("%d refusal log lines, want 1", len(entries))
	}
	f := entries[0].ContextMap()
	if f["reason"] != ReasonPerClient || f["client"] != "2001:db8:1:2::/64" || f["host"] != "example.org" || f["status"] != int64(429) {
		t.Fatalf("log fields: %v", f)
	}
}

func TestHandlerWaiterServedWhenSlotFrees(t *testing.T) {
	h, _ := provisioned(t, &Handler{PerClient: 1, Wait: caddy.Duration(2 * time.Second)})
	up := newBlockingUpstream()
	_, d1 := serve(h, request("203.0.113.7", "example.org"), up)
	<-up.entered
	rec2, d2 := serve(h, request("203.0.113.7", "example.org"), up)
	time.Sleep(30 * time.Millisecond)
	close(up.release) // the first finishes, the second gets its slot
	if err := <-d1; err != nil {
		t.Fatal(err)
	}
	if err := <-d2; err != nil {
		t.Fatal(err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("waiting request: %d, want 200", rec2.Code)
	}
}

func TestHandlerHostCap503(t *testing.T) {
	h, logs := provisioned(t, &Handler{PerClient: 2, MaxPerHost: 3, Wait: caddy.Duration(10 * time.Millisecond)})
	up := newBlockingUpstream()
	var ds []chan error
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		_, d := serve(h, request(ip, "example.org"), up)
		ds = append(ds, d)
		<-up.entered
	}

	rec := httptest.NewRecorder()
	if err := h.ServeHTTP(rec, request("198.51.100.4", "example.org"), up); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fourth client: %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "5" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", rec.Header())
	}

	// The cap is per host: another host still gets through.
	_, d := serve(h, request("198.51.100.4", "other.example"), up)
	ds = append(ds, d)
	select {
	case <-up.entered:
	case <-time.After(time.Second):
		t.Fatal("another host was capped too")
	}

	close(up.release)
	for _, d := range ds {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(logs.FilterField(zap.String("reason", ReasonPerHost)).All()); n != 1 {
		t.Fatalf("%d per_host log lines, want 1", n)
	}
	// The refused client's slot was given back: it can go again.
	if held, _ := h.state.clients.inFlight("198.51.100.4"); held != 0 {
		t.Fatalf("refused client still holds %d slots", held)
	}
	if h.state.clients.size() != 0 || h.state.hosts.size() != 0 {
		t.Fatal("state not empty after every request finished")
	}
}

func TestHandlerOffIsPassThrough(t *testing.T) {
	h, _ := provisioned(t, &Handler{PerClient: 0, MaxPerHost: 0})
	up := newBlockingUpstream()
	close(up.release)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			if err := h.ServeHTTP(rec, request("203.0.113.7", "example.org"), up); err != nil || rec.Code != 200 {
				t.Errorf("%v %d", err, rec.Code)
			}
		}()
	}
	wg.Wait()
	if h.state != nil {
		t.Fatal("a switched-off handler keeps state")
	}
}

func TestHandlerCancelledWaiterGetsNoResponse(t *testing.T) {
	h, logs := provisioned(t, &Handler{PerClient: 1, Wait: caddy.Duration(time.Minute)})
	up := newBlockingUpstream()
	_, d1 := serve(h, request("203.0.113.7", "example.org"), up)
	<-up.entered
	ctx, cancel := context.WithCancel(context.Background())
	r := request("203.0.113.7", "example.org").WithContext(ctx)
	r = r.WithContext(context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{caddyhttp.ClientIPVarKey: "203.0.113.7"}))
	_, d2 := serve(h, r, up)
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-d2; err == nil {
		t.Fatal("cancelled waiter returned no error")
	}
	close(up.release)
	<-d1
	if logs.Len() != 0 {
		t.Fatal("a client that went away was logged as refused")
	}
}

func TestStateSurvivesReload(t *testing.T) {
	h1, _ := provisioned(t, &Handler{PerClient: 1, Wait: -1})
	up := newBlockingUpstream()
	_, d1 := serve(h1, request("203.0.113.7", "example.org"), up)
	<-up.entered

	// A reload provisions a new instance with the same settings before the
	// old one is cleaned up.
	h2, _ := provisioned(t, &Handler{PerClient: 1, Wait: -1})
	rec := httptest.NewRecorder()
	if err := h2.ServeHTTP(rec, request("203.0.113.7", "example.org"), up); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after reload: %d, want 429 (the old instance's request still runs)", rec.Code)
	}
	close(up.release)
	<-d1
}

func TestUnmarshalCaddyfile(t *testing.T) {
	d := caddyfile.NewTestDispenser(`inflight_limit {
		per_client 3
		wait 500ms
		max_per_host 8
		retry_after 10
		ipv4_prefix 24
		ipv6_prefix 56
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.PerClient != 3 || time.Duration(h.Wait) != 500*time.Millisecond || h.MaxPerHost != 8 ||
		time.Duration(h.RetryAfter) != 10*time.Second || h.IPv4Prefix != 24 || h.IPv6Prefix != 56 {
		t.Fatalf("%+v", h)
	}

	var def Handler
	if err := def.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`inflight_limit`)); err != nil {
		t.Fatal(err)
	}
	if def.PerClient != DefaultPerClient {
		t.Fatalf("default per_client %d", def.PerClient)
	}

	var off Handler
	if err := off.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`inflight_limit {
		per_client 0
		wait 0
	}`)); err != nil {
		t.Fatal(err)
	}
	if off.enabled() || off.Wait >= 0 {
		t.Fatalf("per_client 0 / wait 0: %+v", off)
	}

	for name, in := range map[string]string{
		"unknown":    "inflight_limit {\n bogus 1\n}",
		"not number": "inflight_limit {\n per_client two\n}",
		"two args":   "inflight_limit {\n per_client 1 2\n}",
		"bad dur":    "inflight_limit {\n wait soon\n}",
		"inline arg": "inflight_limit 2",
	} {
		var x Handler
		if err := x.UnmarshalCaddyfile(caddyfile.NewTestDispenser(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateRejectsBadPrefixes(t *testing.T) {
	for _, h := range []Handler{
		{PerClient: 1, IPv4Prefix: 33, IPv6Prefix: 64},
		{PerClient: 1, IPv4Prefix: 32, IPv6Prefix: 129},
		{PerClient: -1, IPv4Prefix: 32, IPv6Prefix: 64},
	} {
		if err := h.Validate(); err == nil {
			t.Errorf("%+v accepted", h)
		}
	}
}
