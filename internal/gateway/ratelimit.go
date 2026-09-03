package gateway

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate-limit defaults. The API tier is generous — a dashboard doing bulk work
// shouldn't notice it — while still capping a runaway client; the auth tier is
// tighter because unauthenticated /auth traffic is where credential-stuffing and
// enumeration land. These are a coarse abuse/DoS guard, not the primary
// brute-force defense (that is bcrypt today and account lockout in the Accounts
// milestone).
const (
	defaultAPIPerMinute  = 6000 // ~100 req/s sustained, per principal or IP
	defaultAPIBurst      = 200
	defaultAuthPerMinute = 60 // per client IP
	defaultAuthBurst     = 20
	// maxRateBuckets bounds limiter memory: past this many distinct keys, idle
	// (refilled-to-full) buckets are swept before a new key is admitted. This
	// keeps an IP-spraying attacker from growing the map without bound.
	maxRateBuckets = 10000
)

// RateLimiter decides whether a request keyed by a string may proceed. It is the
// seam a distributed backend (e.g. Redis) drops into later without touching the
// middleware or the call sites — the in-process token bucket below is the only
// implementation today, correct for the single-process, backend-per-customer
// topology DCMS targets. When it denies, retryAfter hints how long until a token
// frees up (for the Retry-After header).
type RateLimiter interface {
	Allow(key string) (allowed bool, retryAfter time.Duration)
}

// RateLimitOptions configures the two limiter tiers. A zero field falls back to
// its default. Passed via gateway.Options.RateLimit; a nil RateLimit disables
// limiting entirely, so the zero-value gateway (and tests) do no limiting.
type RateLimitOptions struct {
	APIPerMinute  int // general collection API, keyed per principal (IP fallback)
	APIBurst      int
	AuthPerMinute int // unauthenticated /auth endpoints, keyed per client IP
	AuthBurst     int
}

func (o RateLimitOptions) withDefaults() RateLimitOptions {
	if o.APIPerMinute <= 0 {
		o.APIPerMinute = defaultAPIPerMinute
	}
	if o.APIBurst <= 0 {
		o.APIBurst = defaultAPIBurst
	}
	if o.AuthPerMinute <= 0 {
		o.AuthPerMinute = defaultAuthPerMinute
	}
	if o.AuthBurst <= 0 {
		o.AuthBurst = defaultAuthBurst
	}
	return o
}

// tokenBucket is one key's allowance: a float count of tokens that refills at a
// steady rate up to a burst ceiling.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// memoryLimiter is an in-process, per-key token-bucket RateLimiter. Buckets are
// created lazily and swept when idle, so memory tracks active keys, not all keys
// ever seen. The clock is injectable for deterministic tests.
type memoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens per second
	burst   float64
	max     int
	now     func() time.Time
}

func newMemoryLimiter(perMinute, burst int) *memoryLimiter {
	return &memoryLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    float64(perMinute) / 60.0,
		burst:   float64(burst),
		max:     maxRateBuckets,
		now:     time.Now,
	}
}

func (l *memoryLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= l.max {
			l.sweep(now)
		}
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Time for the bucket to earn the one token this request needs.
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled to full — i.e. keys idle long enough to
// be indistinguishable from a fresh one, so forgetting them changes nothing.
func (l *memoryLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// rateLimit builds a middleware that admits or rejects each request by the given
// limiter, keyed by key(r). A rejection is a 429 with a Retry-After header.
func (s *Server) rateLimit(l RateLimiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retry := l.Allow(key(r)); !ok {
				secs := int(math.Ceil(retry.Seconds()))
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeError(w, http.StatusTooManyRequests, apiError{Code: "RATE_LIMITED", Message: "too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// apiRateKey keys the general API limiter: an authenticated caller gets their own
// bucket (fair across shared IPs), everyone else falls back to their client IP.
func (s *Server) apiRateKey(r *http.Request, trustProxy bool) string {
	if p := principalFromContext(r.Context()); p.Authenticated && p.ID != "" {
		return "u:" + p.ID
	}
	return "ip:" + clientIP(r, trustProxy)
}

// clientIP extracts the request's client IP for keying. With trustProxy it honors
// the left-most X-Forwarded-For entry (the original client as seen by a trusted
// proxy); otherwise it uses the socket peer, which cannot be spoofed.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
