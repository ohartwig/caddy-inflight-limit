// SPDX-FileCopyrightText: 2026 Kai Ole Hartwig <mail@ole-hartwig.eu>
// SPDX-License-Identifier: Apache-2.0

package inflightlimit

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Default prefix lengths of a client key: one IPv4 address is one client,
// one IPv6 /64 is one client (a /64 is the smallest network usually handed
// to a subscriber, and one host can send from any address in it).
const (
	DefaultIPv4Prefix = 32
	DefaultIPv6Prefix = 64
)

func validatePrefixes(v4, v6 int) error {
	if v4 < 1 || v4 > 32 {
		return fmt.Errorf("ipv4_prefix must be 1-32, got %d", v4)
	}
	if v6 < 1 || v6 > 128 {
		return fmt.Errorf("ipv6_prefix must be 1-128, got %d", v6)
	}
	return nil
}

// clientKey returns the client's address masked to its network. It reads
// Caddy's client IP, so trusted_proxies and client_ip_headers apply. A full
// prefix (32, 128) gives the bare address; a shorter one the network in CIDR
// form ("2001:db8:1:2::/64"). An address that does not parse is returned as
// it is, so distinct clients never fall into one shared key.
func clientKey(r *http.Request, v4, v6 int) string {
	raw, _ := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string)
	if raw == "" {
		raw = r.RemoteAddr
		if host, _, err := net.SplitHostPort(raw); err == nil {
			raw = host
		}
	}
	return maskAddr(raw, v4, v6)
}

func maskAddr(raw string, v4, v6 int) string {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	addr = addr.Unmap().WithZone("")
	bits := v6
	if addr.Is4() {
		bits = v4
	}
	if bits >= addr.BitLen() {
		return addr.String()
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}

func intArg(d *caddyfile.Dispenser) (int, error) {
	name := d.Val()
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	n, err := strconv.Atoi(d.Val())
	if err != nil {
		return 0, d.Errf("%s: not a number: %s", name, d.Val())
	}
	if d.NextArg() {
		return 0, d.ArgErr()
	}
	return n, nil
}
