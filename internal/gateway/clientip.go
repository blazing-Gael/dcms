package gateway

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Client IP and forwarded-header trust (GHSA-f8wf-92mm-pw4g).
//
// Each proxy appends the address it received a request from to X-Forwarded-For, so
// only entries added by the operator's own proxies are trustworthy; anything to
// their left may be client-supplied. DCMS therefore:
//
//  1. honours forwarded headers only when TrustProxy is on AND the connection comes
//     from a trusted proxy (so a client reaching the port directly can't spoof);
//  2. walks X-Forwarded-For right to left, skipping trusted proxy addresses, and
//     takes the first untrusted one as the client.
//
// That works for any proxy depth without counting hops. Behind Cloudflare the
// operator lists Cloudflare's ranges in TrustedProxies, or names CF-Connecting-IP
// in ClientIPHeader.

// defaultTrustedProxies covers proxies on the same host or a private network:
// loopback, RFC 1918, and IPv6 unique-local — nginx/Caddy beside DCMS, Docker
// networks, a LAN or VPC load balancer.
var defaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// trustedProxy reports whether addr is one of the operator's proxies.
func (s *Server) trustedProxy(addr netip.Addr) bool {
	return inTrustedRanges(s.opts.TrustedProxies, addr)
}

// inTrustedRanges reports whether addr falls in ranges (nil ⇒ the defaults).
func inTrustedRanges(ranges []netip.Prefix, addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if ranges == nil {
		ranges = defaultTrustedProxies
	}
	for _, p := range ranges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// peerAddr parses the connection's remote address.
func peerAddr(r *http.Request) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr, host
}

// forwardedTrusted reports whether this request's forwarded headers may be read:
// proxy trust is on and the connection comes from a trusted proxy.
func (s *Server) forwardedTrusted(r *http.Request) bool {
	if !s.opts.TrustProxy {
		return false
	}
	addr, _ := peerAddr(r)
	return s.trustedProxy(addr)
}

// clientIP returns the request's client IP for rate-limit keying: the socket
// peer, unless the peer is a trusted proxy, in which case ClientIPHeader or the
// right-most untrusted X-Forwarded-For entry.
func (s *Server) clientIP(r *http.Request) string {
	addr, host := peerAddr(r)
	if addr.IsValid() {
		host = addr.Unmap().String()
	}
	if !s.forwardedTrusted(r) {
		return host
	}
	if h := s.opts.ClientIPHeader; h != "" {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(h))); err == nil {
			return a.Unmap().String()
		}
		// Header missing or malformed: fall back to X-Forwarded-For.
	}
	// Join every header line: some proxies (HAProxy's `option forwardfor`) add their
	// own X-Forwarded-For line instead of appending, and Header.Get would return
	// only the first — the client-supplied one.
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	leftmost := ""
	for i := len(parts) - 1; i >= 0; i-- {
		entry := strings.TrimSpace(parts[i])
		if entry == "" {
			continue
		}
		a, err := netip.ParseAddr(entry)
		if err != nil {
			// Not an IP, so it can't be one of our proxies: it is the first entry we
			// can't vouch for. Key on it rather than skip past it to a forged value.
			return entry
		}
		if !s.trustedProxy(a) {
			return a.Unmap().String()
		}
		leftmost = a.Unmap().String()
	}
	if leftmost != "" {
		return leftmost // every hop is a trusted address (an internal client)
	}
	return host
}
