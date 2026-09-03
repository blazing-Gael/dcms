// Package auth defines the public seam through which identity sources plug into
// DCMS (ADR-0016 milestone 4, ADR-0020). DCMS does authorization; who a caller
// *is* is decided by an Authenticator, and the built-in opaque-session
// implementation is only one of them.
//
// A host embedding DCMS — or bringing its own identity source (OIDC, a reverse
// proxy that injects a verified header, a third-party auth service such as
// Appwrite) — implements Authenticator, hands DCMS a verified Principal, and lets
// the existing `access:` machinery enforce policy. The engine never learns which
// source authenticated a request.
//
// This package deliberately has no dependency on the rest of DCMS, so it can be
// imported by external code without pulling in the engine.
package auth

import (
	"context"
	"net/http"
	"slices"
)

// Principal is the verified identity behind a request: who they are (ID) and what
// roles they hold. The zero value — Authenticated == false — is an anonymous
// caller.
//
// A Principal is the *verified* identity; it (and only it) is what DCMS uses for
// audit attribution (created_by/updated_by), so an Authenticator must never
// populate it from unverified client input.
type Principal struct {
	// ID is the stable identifier of the caller within DCMS. For the built-in
	// session source it is the _users row id; for an external source it is
	// whatever id that source maps to (often via a linked identity record).
	ID string

	// Roles are the role names the caller holds. DCMS's `access:` rules are
	// evaluated against these; the engine assigns no meaning to a role beyond
	// what a schema's rules give it.
	Roles []string

	// Authenticated distinguishes a real identity from the anonymous zero value.
	// An Authenticator sets it true only for a request it actually verified.
	Authenticated bool

	// Claims carries identity-source-specific data the host wants to keep — an
	// external subject id, a tenant, an email, group memberships. The engine does
	// not interpret Claims; it is available to hosts and to future policy rules
	// so external identity need not be smuggled into role strings. May be nil.
	Claims map[string]string
}

// HasRole reports whether the Principal holds the named role.
func (p Principal) HasRole(role string) bool {
	return slices.Contains(p.Roles, role)
}

// Authenticator resolves an *http.Request into a Principal. It is the single seam
// where identity sources plug in.
//
// A returned error is a hard failure (e.g. the identity source is unreachable),
// not "not logged in": an anonymous request returns the zero Principal and a nil
// error. This lets the gateway distinguish "no credentials" (proceed as
// anonymous) from "auth backend is down" (fail the request).
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

type contextKey struct{}

// NewContext returns a child context carrying p. DCMS's middleware calls this
// once per request; a host wiring its own middleware around the gateway can call
// it too, so long as it uses this package's helper (the context key is
// unexported, so FromContext and NewContext are the only way in and out).
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext returns the Principal placed in ctx by NewContext, or the zero
// (anonymous) Principal if none was set.
func FromContext(ctx context.Context) Principal {
	p, _ := ctx.Value(contextKey{}).(Principal)
	return p
}
