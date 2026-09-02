package gateway

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSOptions configures cross-origin resource sharing. It is applied only when
// gateway.Options.CORS is non-nil; an empty AllowedOrigins then denies all
// cross-origin requests (same-origin still works). A single "*" entry allows any
// origin — but never together with AllowCredentials, since the browser forbids a
// wildcard origin on a credentialed response.
type CORSOptions struct {
	AllowedOrigins   []string
	AllowedMethods   []string
	AllowedHeaders   []string
	ExposedHeaders   []string
	AllowCredentials bool
	MaxAgeSeconds    int
}

// default CORS values, applied per-field when unset. The request headers cover
// everything the API reads (auth, JSON, idempotency, conditional GET, preview);
// the exposed headers cover what a browser client needs to read back.
var (
	defaultCORSMethods = []string{
		http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	defaultCORSAllowedHeaders = []string{
		"Authorization", "Content-Type", "Idempotency-Key", "If-None-Match", "X-DCMS-Preview",
	}
	defaultCORSExposedHeaders = []string{
		"ETag", "Retry-After", "Idempotent-Replay",
	}
)

const defaultCORSMaxAge = 600 // 10 minutes of preflight caching

func (o CORSOptions) withDefaults() CORSOptions {
	if len(o.AllowedMethods) == 0 {
		o.AllowedMethods = defaultCORSMethods
	}
	if len(o.AllowedHeaders) == 0 {
		o.AllowedHeaders = defaultCORSAllowedHeaders
	}
	if len(o.ExposedHeaders) == 0 {
		o.ExposedHeaders = defaultCORSExposedHeaders
	}
	if o.MaxAgeSeconds == 0 {
		o.MaxAgeSeconds = defaultCORSMaxAge
	}
	return o
}

// wildcard reports whether any origin is allowed. A wildcard is honored only for
// non-credentialed requests; with credentials the matched origin is echoed
// instead (a wildcard is invalid on a credentialed response).
func (o CORSOptions) wildcard() bool {
	for _, x := range o.AllowedOrigins {
		if x == "*" {
			return true
		}
	}
	return false
}

func (o CORSOptions) originAllowed(origin string) bool {
	if o.wildcard() {
		return true
	}
	for _, x := range o.AllowedOrigins {
		if strings.EqualFold(x, origin) {
			return true
		}
	}
	return false
}

// middleware returns a CORS handler. It adds the response headers for an allowed
// cross-origin request and short-circuits a preflight OPTIONS with 204, so the
// preflight never reaches auth, the route-group limiters, or a handler.
func (o CORSOptions) middleware() func(http.Handler) http.Handler {
	allowMethods := strings.Join(o.AllowedMethods, ", ")
	allowHeaders := strings.Join(o.AllowedHeaders, ", ")
	exposeHeaders := strings.Join(o.ExposedHeaders, ", ")
	maxAge := strconv.Itoa(o.MaxAgeSeconds)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r) // not a cross-origin (browser) request
				return
			}
			if !o.originAllowed(origin) {
				// Disallowed origin: add no CORS headers. A preflight still returns
				// 204 (with no allow headers, so the browser blocks it); an actual
				// request proceeds but the browser will withhold the response.
				if isPreflight(r) {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			if o.wildcard() && !o.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				// Echo the specific origin (required with credentials) and vary on
				// it so caches don't serve one origin's response to another.
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			if o.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if len(o.ExposedHeaders) > 0 {
				h.Set("Access-Control-Expose-Headers", exposeHeaders)
			}

			if isPreflight(r) {
				h.Set("Access-Control-Allow-Methods", allowMethods)
				h.Set("Access-Control-Allow-Headers", allowHeaders)
				h.Set("Access-Control-Max-Age", maxAge)
				h.Add("Vary", "Access-Control-Request-Method")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isPreflight reports whether r is a CORS preflight: an OPTIONS carrying the
// Access-Control-Request-Method header.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}
