// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

package inflightlimit_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	_ "github.com/ohartwig/caddy-inflight-limit"
)

// A real Caddy from a Caddyfile: trusted_proxies resolves the client from
// X-Forwarded-For, inflight_limit sits in a route in front of reverse_proxy
// to an upstream that holds /slow until released.
func TestIntegration(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	entered := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			entered <- r.Header.Get("X-Forwarded-For")
			<-release
		}
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 9080
		https_port 9443
		grace_period 1ns
		servers {
			trusted_proxies static 127.0.0.1/32 ::1/128
			client_ip_headers X-Forwarded-For
		}
	}
	http://localhost:9080 {
		route {
			inflight_limit {
				per_client 2
				wait 200ms
				max_per_host 3
				retry_after 7
			}
			reverse_proxy %s
		}
	}`, strings.TrimPrefix(upstream.URL, "http://")), "caddyfile")

	get := func(path, xff string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, "http://localhost:9080"+path, nil)
		req.Header.Set("X-Forwarded-For", xff)
		res, err := tester.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	async := func(path, xff string) chan *http.Response {
		ch := make(chan *http.Response, 1)
		go func() { ch <- get(path, xff) }()
		return ch
	}

	res := get("/fast", "2001:db8:1:2::1")
	if res.StatusCode != 200 {
		t.Fatalf("fast: %d", res.StatusCode)
	}

	// Two requests of one /64 fill its slots.
	a := async("/slow", "2001:db8:1:2::1")
	b := async("/slow", "2001:db8:1:2::2")
	<-entered
	<-entered

	// The third of that /64 waits 200 ms, then 429.
	start := time.Now()
	res = get("/slow", "2001:db8:1:2:ffff::3")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third of the /64: %d, want 429", res.StatusCode)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("429 after %v, before the wait", d)
	}
	if res.Header.Get("Retry-After") != "7" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("429 headers: %v", res.Header)
	}

	// An IPv4 client takes the host's third slot ...
	c := async("/slow", "198.51.100.9")
	<-entered
	// ... and the fourth request of the host is refused at once with 503.
	start = time.Now()
	res = get("/slow", "198.51.100.10")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over the host cap: %d, want 503", res.StatusCode)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("503 took %v; the host cap does not wait", d)
	}

	once.Do(func() { close(release) })
	for _, ch := range []chan *http.Response{a, b, c} {
		if r := <-ch; r.StatusCode != 200 {
			t.Fatalf("held request: %d", r.StatusCode)
		}
	}

	// Everything finished: the same /64 gets through again.
	if r := get("/fast", "2001:db8:1:2::1"); r.StatusCode != 200 {
		t.Fatalf("after release: %d", r.StatusCode)
	}
}
