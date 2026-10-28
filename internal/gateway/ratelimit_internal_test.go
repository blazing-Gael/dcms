package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// The token bucket admits up to `burst` immediately, then denies until it refills
// at the configured rate; a refill restores an allowance. Driven by a fake clock
// so it's exact, not timing-dependent.
func TestMemoryLimiter_BurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := newMemoryLimiter(60, 3) // 1 token/sec, burst 3
	l.now = func() time.Time { return now }

	// Burst of 3 passes.
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	// 4th is denied, with a ~1s retry hint (one token at 1/sec).
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("request past burst should be denied")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retryAfter should be ~1s, got %v", retry)
	}

	// After 1s a single token is back: exactly one more passes, the next denied.
	now = now.Add(time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("one token should have refilled after 1s")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("only one token should have refilled")
	}
}

// Distinct keys have independent buckets — one client exhausting its allowance
// never throttles another.
func TestMemoryLimiter_KeysAreIndependent(t *testing.T) {
	now := time.Unix(0, 0)
	l := newMemoryLimiter(60, 1)
	l.now = func() time.Time { return now }

	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("first hit on a should pass")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("second hit on a should be denied (burst 1)")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b has its own bucket and should pass")
	}
}

// Idle (refilled-to-full) buckets are swept once the map is over capacity, so an
// IP-spraying client can't grow the map without bound.
func TestMemoryLimiter_SweepsIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newMemoryLimiter(60, 1)
	l.now = func() time.Time { return now }
	l.max = 2

	l.Allow("a") // 'a' spends its token, so it is NOT full/idle
	l.Allow("b") // 'b' likewise; map now at max (2)
	now = now.Add(time.Hour)
	// 'a' and 'b' have long since refilled to full → both idle. Admitting 'c'
	// triggers a sweep that reclaims them.
	l.Allow("c")
	if len(l.buckets) > 2 {
		t.Fatalf("idle buckets should have been swept, map has %d", len(l.buckets))
	}
}

// TestClientIP pins GHSA-f8wf-92mm-pw4g: forwarded headers count only from a
// trusted proxy, and the client is the right-most X-Forwarded-For entry that isn't
// one of ours — so a client-supplied entry on the left never becomes the key.
func TestClientIP(t *testing.T) {
	cloudflare := []netip.Prefix{netip.MustParsePrefix("173.245.48.0/20")}
	cases := []struct {
		name    string
		opts    Options
		peer    string
		xff     []string // one element per header line
		headers map[string]string
		want    string
	}{
		{name: "trust off ignores XFF", opts: Options{}, peer: "203.0.113.7:1", xff: []string{"6.6.6.6"}, want: "203.0.113.7"},
		{name: "untrusted peer ignores XFF (direct hit on the port)", opts: Options{TrustProxy: true}, peer: "203.0.113.7:1", xff: []string{"6.6.6.6"}, want: "203.0.113.7"},
		{name: "nginx on localhost", opts: Options{TrustProxy: true}, peer: "127.0.0.1:1", xff: []string{"203.0.113.7"}, want: "203.0.113.7"},
		{name: "spoofed left entry skipped", opts: Options{TrustProxy: true}, peer: "127.0.0.1:1", xff: []string{"1.2.3.4, 203.0.113.7"}, want: "203.0.113.7"},
		{name: "two internal hops", opts: Options{TrustProxy: true}, peer: "10.0.0.3:1", xff: []string{"1.2.3.4, 203.0.113.7, 10.0.0.2"}, want: "203.0.113.7"},
		{name: "proxy adds its own header line (HAProxy)", opts: Options{TrustProxy: true}, peer: "10.0.0.3:1", xff: []string{"6.6.6.6", "203.0.113.7"}, want: "203.0.113.7"},
		{name: "cloudflare edge then nginx", opts: Options{TrustProxy: true, TrustedProxies: append([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, cloudflare...)},
			peer: "127.0.0.1:1", xff: []string{"6.6.6.6, 203.0.113.7, 173.245.48.9"}, want: "203.0.113.7"},
		{name: "cloudflare ranges not configured: edge is the client", opts: Options{TrustProxy: true}, peer: "127.0.0.1:1", xff: []string{"203.0.113.7, 173.245.48.9"}, want: "173.245.48.9"},
		{name: "client_ip_header from trusted peer", opts: Options{TrustProxy: true, ClientIPHeader: "CF-Connecting-IP"}, peer: "127.0.0.1:1",
			xff: []string{"6.6.6.6"}, headers: map[string]string{"CF-Connecting-IP": "203.0.113.7"}, want: "203.0.113.7"},
		{name: "client_ip_header from untrusted peer ignored", opts: Options{TrustProxy: true, ClientIPHeader: "CF-Connecting-IP"}, peer: "198.51.100.1:1",
			headers: map[string]string{"CF-Connecting-IP": "6.6.6.6"}, want: "198.51.100.1"},
		{name: "every hop internal", opts: Options{TrustProxy: true}, peer: "127.0.0.1:1", xff: []string{"10.1.2.3"}, want: "10.1.2.3"},
		{name: "garbage entry is not skipped past", opts: Options{TrustProxy: true}, peer: "127.0.0.1:1", xff: []string{"6.6.6.6, garbage"}, want: "garbage"},
		{name: "ipv4-mapped peer", opts: Options{TrustProxy: true}, peer: "[::ffff:127.0.0.1]:1", xff: []string{"203.0.113.7"}, want: "203.0.113.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.RemoteAddr = tc.peer
			for _, line := range tc.xff {
				r.Header.Add("X-Forwarded-For", line)
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			s := &Server{opts: tc.opts}
			if got := s.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
