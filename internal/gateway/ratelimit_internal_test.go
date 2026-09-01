package gateway

import (
	"net/http"
	"net/http/httptest"
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

func TestClientIP(t *testing.T) {
	base := httptest.NewRequest(http.MethodGet, "/x", nil)
	base.RemoteAddr = "203.0.113.7:54321"

	if got := clientIP(base, false); got != "203.0.113.7" {
		t.Fatalf("no-proxy: got %q, want the socket IP", got)
	}

	// trustProxy off: X-Forwarded-For is ignored (unspoofable keying).
	base.Header.Set("X-Forwarded-For", "198.51.100.9, 203.0.113.7")
	if got := clientIP(base, false); got != "203.0.113.7" {
		t.Fatalf("no-proxy should ignore XFF, got %q", got)
	}
	// trustProxy on: the left-most XFF entry (original client) is used.
	if got := clientIP(base, true); got != "198.51.100.9" {
		t.Fatalf("proxy: got %q, want the left-most XFF entry", got)
	}
}
