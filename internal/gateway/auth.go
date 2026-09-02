package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/blazing-Gael/dcms/internal/store"
)

// principal is the authenticated identity behind a request (ADR-0016): who they
// are (id) and what roles they hold. The zero value — authenticated == false —
// is an anonymous caller. It is resolved once by withPrincipal and read by every
// authorization check; it is the *verified* identity, so it (and only it) is what
// feeds store.WithActor for audit attribution.
type principal struct {
	id            string
	roles         []string
	authenticated bool
}

// hasRole reports whether the principal holds the named role.
func (p principal) hasRole(role string) bool {
	for _, r := range p.roles {
		if r == role {
			return true
		}
	}
	return false
}

// Authenticator resolves an *http.Request into a principal. It is the single seam
// where identity sources plug in: M1 ships a session-backed implementation
// (sessionAuthenticator); external OIDC/JWKS (a later milestone) is another
// implementation of this same interface, so the rest of the gateway never learns
// which source authenticated a request.
//
// A returned error is a hard failure (e.g. the store is unreachable), not "not
// logged in" — an anonymous request returns the zero principal and a nil error.
type Authenticator interface {
	Authenticate(r *http.Request) (principal, error)
}

type principalCtxKey struct{}

// withPrincipal resolves the request's principal once and stashes it in context,
// mirroring withVisibility. When a principal is authenticated it also seeds
// store.WithActor from that verified id, so created_by/updated_by attribution can
// never come from client input. A nil Authenticator means auth is not configured
// (e.g. in tests): the request proceeds with no principal and no actor, and the
// authorization checks short-circuit to allow (see authEnabled).
func (s *Server) withPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Authenticator == nil {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.opts.Authenticator.Authenticate(r)
		if err != nil {
			s.logger.Error("authenticator failure", "err", err, "path", r.URL.Path)
			writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		if p.authenticated {
			ctx = store.WithActor(ctx, p.id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFromContext returns the principal resolved by withPrincipal, or the
// zero (anonymous) principal if none was set.
func principalFromContext(ctx context.Context) principal {
	p, _ := ctx.Value(principalCtxKey{}).(principal)
	return p
}

// authEnabled reports whether authorization is enforced. It is off exactly when
// no Authenticator is configured, which keeps the pre-auth behavior for callers
// (and tests) that don't wire one. Production always wires one.
func (s *Server) authEnabled() bool { return s.opts.Authenticator != nil }

// requestIsSecure reports whether the request reached the client over HTTPS —
// directly, or (when TrustProxy is set) via a proxy that terminated TLS and set
// X-Forwarded-Proto. It drives the session cookie's Secure flag, so a
// proxy-terminated deployment still marks the cookie HTTPS-only.
func (s *Server) requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.opts.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
