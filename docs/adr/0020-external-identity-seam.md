# ADR-0020 — Public external-identity seam (`pkg/auth`)

Status: Accepted
Date: 2026-09-03

## Context

ADR-0016 named the `Authenticator` interface as "the single extension point where
identity sources plug in", and named milestone 4 "External identity — OIDC/JWKS".
Both the README and ADR-0003 promise "bring your own auth": DCMS does
authorization, and *who a caller is* can be decided by something other than the
built-in opaque-session store.

That promise could not actually be kept from outside the module (reported as
issue #1, by a maintainer of a downstream site whose identity lives in Appwrite):

- `Authenticator` lived in `internal/gateway`, so external packages could not
  import it at all.
- Its method returned the **unexported** `principal`, so even a moved package
  could not produce a value satisfying the interface.

Net effect: any external identity source required a fork. This ADR closes that
gap. It is deliberately scoped to **exposing the seam** — it does *not* add OIDC,
JWKS, or any provider-specific code. A generic, config-driven OIDC relying-party
built on this seam is a separate, later decision ("Layer 2"); by keeping it out
of this ADR, external auth becomes possible today with zero new dependencies, and
provider-specific behavior (Appwrite's API-only validation, non-OIDC OAuth) stays
out-of-tree where it belongs.

## Decision

### 1. Promote `Principal` and `Authenticator` to a public package

A new `pkg/auth` holds the seam, with **no dependency on the rest of DCMS** so
external code can import it without pulling in the engine:

```go
package auth

type Principal struct {
	ID            string
	Roles         []string
	Authenticated bool
	Claims        map[string]string // identity-source-specific; engine does not interpret it
}

func (p Principal) HasRole(role string) bool

type Authenticator interface {
	// Anonymous request → zero Principal, nil error.
	// Error → hard failure (identity source unreachable), not "not logged in".
	Authenticate(r *http.Request) (Principal, error)
}
```

The anonymous-vs-error contract is unchanged from ADR-0016: it lets the gateway
distinguish "no credentials, proceed as anonymous" from "auth backend is down,
fail the request".

### 2. `Claims` — carry external identity without abusing roles

An OIDC/Appwrite subject rarely maps to *just* an id and a role list. `Claims` is
an opaque `map[string]string` the Authenticator may populate (external subject,
tenant, email, group memberships). The engine assigns it no meaning; it is there
so hosts — and future policy rules — need not smuggle identity into role strings.
It is additive and defaults to nil; the built-in session source leaves it unset.

### 3. One shared context key: `auth.NewContext` / `auth.FromContext`

The principal is stored in `context.Context` under a key **owned by `pkg/auth`**,
reachable only through `NewContext` (write) and `FromContext` (read). The
gateway's middleware writes with `NewContext`; a host wiring its own middleware
*around* the gateway reads the same value with `FromContext`. There is exactly
one key, so the two cannot diverge.

### 4. The gateway consumes the public types; nothing internal forks

`internal/gateway` keeps the short spellings via aliases —
`type principal = auth.Principal`, `type Authenticator = auth.Authenticator` — so
the ~30 internal call sites and the exported `gateway.Options.Authenticator` /
`gateway.NewSessionAuthenticator` API are unchanged for existing callers. The
verified `Principal` remains the sole source of `store.WithActor` attribution
(audit stamping never comes from client input), exactly as before.

Because `principal` is now the public struct, its fields are the exported
`ID`/`Roles`/`Authenticated` and the method is `HasRole`; that is the only
internal churn, and it is mechanical.

## Consequences

- **Bring-your-own-auth works out-of-tree, today.** A host implements
  `auth.Authenticator` (≈40 lines), sets `gateway.Options.Authenticator`, and its
  verified `Principal` drives the existing `access:` machinery unchanged — proven
  end-to-end by `TestExternalAuthenticator_DrivesAuthorization`, which uses only
  `pkg/auth` (no gateway internals).
- **The Appwrite / resource-server pattern is unblocked** without DCMS taking on
  any provider code: the host verifies the external token however that provider
  requires and hands DCMS a `Principal`. A JIT-provisioned, passwordless `_users`
  row (ADR-0019) links the external subject; the `hash == ""` login guard already
  prevents such accounts from the local-login path.
- **No new dependencies, no new endpoints, no enforcement change.** This is purely
  a visibility/typing change to a seam ADR-0016 already designed.
- **Layer 2 (generic OIDC) is deferred, not precluded.** A config-driven OIDC
  verifier + authorization-code/PKCE login flow — driven by issuer discovery so
  no provider needs bespoke code — can be built on this seam later; it is the one
  piece that would add `go-oidc`/`oauth2`. That decision is out of scope here.

## Alternatives considered

- **Keep the seam internal, add providers in core.** Rejected: it fails the
  bring-your-own promise, and it puts provider-specific code (which for non-OIDC
  sources like Appwrite is unavoidable) into the engine.
- **A distinct public type converted to/from an internal `principal`.** Rejected:
  two structs and a conversion at every boundary, and `auth.FromContext` could not
  see what the gateway stored. A single aliased type with one context key is
  simpler and is what makes `FromContext` usable by external middleware.
