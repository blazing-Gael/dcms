# ADR-0016 — Authentication & authorization: authz spine + opaque local sessions

Status: Accepted
Date: 2026-07-18

## Context

DCMS has enforced *lifecycle* visibility at the gateway (ADR-0012) and stamps a
`created_by`/`updated_by` actor from context (ADR-0005), but it has had no real
concept of **who** a caller is or **what** they are allowed to do. Every request
has been effectively an anonymous admin. This ADR introduces authentication
(authn — proving who you are) and authorization (authz — deciding what you may
do), the first security-bearing layer of the engine.

Constraints inherited from earlier decisions frame the whole design:

- **The store is auth-agnostic and method-locked** (ADR-0003). No authz logic may
  live in the store, and no new store *methods* may be added. Users, sessions,
  and roles are ordinary records reached through the existing CRUD surface.
- **Actor stamping is attribution only** (ADR-0005). `store.WithActor` answers
  "who did this" for audit; it is *never* an authorization decision and is set
  only by trusted callers, never from client input. Authz is a separate gate
  that runs *before* the actor is trusted.
- **Backend-per-customer, single-tenant** (ADR-0007). There is no horizontally
  scaled fleet and no cross-tenant surface. This is decisive for the session vs
  JWT choice below.
- **Secrets are env-only** until config-file interpolation exists (ADR-0009).
  Signing secrets, admin bootstrap credentials, and the like never sit in a
  committed config file.
- **Contracts derive from the schema** (ADR-0008). The `access:` block already
  reserved in SCHEMA_SPEC becomes live and flows into OpenAPI/SDK.

## Decision

### 1. authn vs authz are separate layers with one meeting point: the principal

A request resolves to a **principal** — `{ ID, Roles, Authenticated }` — exactly
once, in a middleware that mirrors `withVisibility` (ADR-0012): resolve, stash in
context, and feed `store.WithActor(ctx, principal.ID)` so audit attribution comes
from the *verified* identity rather than anything the client asserted. Every
downstream layer (authz checks, handlers, expansion) reads the one principal.

The principal is produced by an **`Authenticator` seam** — an interface that turns
an `*http.Request` into a principal. This is the single extension point where
identity sources plug in. M1 ships one implementation (opaque local sessions);
external OIDC/JWKS (M3) is another implementation of the *same* seam, so the rest
of the engine never learns which identity source authenticated a request.

### 2. Local identity uses opaque, DB-backed sessions — not JWT

Login exchanges credentials for a **random opaque token**; the server stores only
its hash in an engine-managed `_sessions` collection and looks it up (one indexed
read) per authenticated request. Rationale, weighted for *this* system:

- **Instant revocation & fresh roles.** Logout, ban, "log out everywhere", and
  role changes take effect on the next request by deleting/reading rows. A CMS
  admin expects this immediacy; it aligns with the audit/trust posture.
- **JWT's marquee advantages don't apply here.** Stateless, shared-nothing
  validation matters for large autoscaled fleets; we are backend-per-customer on
  one DB the request already hits. The extra indexed lookup is negligible next to
  the data query and is *constant* per request (no N+1).
- **Simpler, correctly.** No access/refresh rotation, refresh-reuse detection, or
  alg-confusion footguns.

This is **not** sessions-vs-JWT for the whole system: when external OIDC lands,
the IdP issues JWTs that we *verify* (JWKS) — federated identity where revocation
is the IdP's concern. The `jwt:` sub-block reserved in SCHEMA_SPEC therefore
pertains to the **external** provider (M3), not to local login. Local login uses
`auth.session` (a TTL). Long-lived machine/API tokens, when needed, will reuse the
opaque-token mechanism (revocable, scopable) rather than un-revocable JWTs.

### 3. Authorization is the `access:` block, evaluated once at the gateway

Each collection may declare per-operation rules:

```yaml
access:
  read:   public                # anyone
  create: [admin, editor]       # a caller holding one of these roles
  update: [admin, editor]
  delete: [admin]
```

Rule grammar (already reserved in SCHEMA_SPEC):

- `public` — no authentication required.
- `authenticated` — any valid principal, regardless of role.
- `[role, …]` — the principal holds at least one listed role.
- `owner` — the principal is the record's `created_by` (record-scoped).
- `any: [rule, …]` — a **composite** OR of the above; the caller satisfies the
  rule if they satisfy any sub-rule. Each element is itself a rule (a keyword, a
  role, a role list, or a nested composite), so `any: [admin, owner]` grants
  admins full access while narrowing everyone else to the rows they own.

The composite is evaluated by combining its sub-decisions with the precedence
`allow > owner-scope > deny`: an admin resolves to a plain *allow* (sees every
row, no `created_by` filter), while a non-admin resolves to *owner-scope* (the
same `created_by` narrowing a bare `owner` rule produces). Because the outcome
is still one of the three decisions the enforcement layer already handles, no
enforcement path changed and no extra query is issued — a list read emits at
most one `created_by` filter, exactly as before. This fills the gap a bare
`owner` rule left: `owner` alone also hid records from admins, which
`any: [admin, owner]` now expresses correctly.

Enforcement lives in the gateway, above the store, and is the mirror image of
lifecycle filtering:

- **Denied write (create/update/delete/transitions) → `403`.** The route exists;
  you may not act on it.
- **Denied single read → `404`.** Never `403` — a `403` leaks that the record
  exists. This matches the lifecycle 404 convention (ADR-0012).
- **`owner` on a *list* read → a `created_by = <principal>` filter** appended to
  the query (same mechanism as `lifecycleFilters`), so a list returns only your
  own rows rather than 403-ing the whole endpoint. `owner` on a single read → the
  record-visibility check (404 if not yours).
- **Denied list read (e.g. role-gated) → `403`.**

Authz runs *before* `store.WithActor` is trusted for the write and before any
lifecycle/reference work, so a forbidden request never reaches the store.

### 4. Default policy when `access:` is omitted

To keep the zero-config path safe-by-default without being unusable:

- **reads → `public`**, **writes (create/update/delete) → `authenticated`.**

A brand-new schema is readable by anyone and writable by any logged-in user, and
tightening is purely additive per collection. The default is itself overridable
by a top-level `auth.default_access` in a later increment; M1 hard-codes the
above. Engine-managed collections are not publicly writable regardless (see §6).

### 5. Roles are schema-declared; the principal carries role strings

`auth.roles` declares the closed set of role names (with labels for the admin UI).
A `_users` record carries a `roles` list; the principal is populated from it at
authentication time. Unknown roles in an `access:` rule are a schema-compile
error (fail fast, mirrors the richtext allowlist approach). There is no role
*hierarchy* in M1 — `[admin]` does not implicitly include a superuser; list every
role that should pass. (Hierarchy/inheritance can come later without breaking the
rule grammar.)

### 6. Engine-managed identity collections: `_users` and `_sessions`

Both are reserved, underscore-prefixed, and injected like `_media`:

- **`_users`** — injected **unconditionally** (identity is a standalone feature; an
  operator can manage users even before writing an `access:` rule). Fields:
  `email` (unique, required), `password_hash`, `roles` (JSON list), `name`,
  plus the always-on audit columns. `password_hash` is **never** serialized in a
  response (stripped at the choke point, like media's `storage_key` in ADR-0015).
- **`_sessions`** — injected whenever local auth is active. Fields: `token_hash`
  (unique), `user_id` (relation → `_users`), `expires_at` (datetime). Rows are
  created at login, deleted at logout, and filtered by `expires_at > now` on
  lookup; expired rows are swept lazily.

Neither is JSON-CRUD routable (the existing `_`-prefix rule in
`routableCollection` already excludes them). `_users` is administered through a
dedicated, authz-gated surface (M1: the login/bootstrap paths and `dcms admin`;
a full users API is a later increment). `_sessions` is never client-writable.

### 7. Auth endpoints and bootstrap

- **`POST /auth/login`** `{email, password}` → sets/returns an opaque session
  token; **`POST /auth/logout`** revokes it; **`GET /auth/me`** returns the current
  principal. These live at the top level (outside `/api/v1`), like `/__media`.
- **Preview token subsumed but retained.** The lifecycle preview bypass (ADR-0012)
  becomes "an authenticated admin sees non-live content"; the env-only
  `DCMS_PREVIEW_TOKEN` stays as a headless escape hatch (CI, static-site build)
  and is unchanged.
- **`/__health` / `/__ready`** stay public (probes).
- **Bootstrap admin.** A `dcms admin create` CLI command writes the first admin
  directly to the store, and on first run an env-seeded admin
  (`DCMS_ADMIN_EMAIL` / `DCMS_ADMIN_PASSWORD`) is created if `_users` is empty —
  so a fresh backend is never locked out and no credential lands in a config file.

## Milestones

1. **(this ADR) Authz spine + local login + bootstrap admin** — principal
   middleware, `access:` parsing + enforcement, opaque sessions, `/auth/*`,
   `dcms admin create` + env seed, `_users`/`_sessions` injection.
2. **Field-level access (delivered)** — the `fields.<name>.access` `read`/`write`
   rules. `read` is a *mask*: an unauthorized reader still gets the record, minus
   the field. `write` is a *filter*: an unauthorized writer's value is silently
   dropped (not rejected), so round-tripping a masked record never 4xxs. Both use
   the same rule grammar as collection access (public/authenticated/roles/owner)
   and are enforced only when auth is enabled and the collection actually declares
   a field rule (`HasFieldAccess`), so the common path pays nothing. Masking runs
   at the serialization choke points (`writeRecord`/`writeRecords`/`coerceExpanded`)
   *after* response-contract validation; write-stripping runs in the create/update
   handlers before validation. An `owner` write rule on update loads the current
   row lazily (once, only when such a field is present) — no extra read otherwise.
   The lazy-load trigger is "the rule tree mentions owner" (`Rule.MentionsOwner`),
   so a composite `any: [admin, owner]` write rule loads the row too.
3. **Composite rules (delivered)** — the `any: [rule, …]` OR combinator (see §3).
   Both collection and field rules accept it; roles named anywhere in the tree are
   validated against `auth.roles`. Zero enforcement-path or query-cost change: the
   evaluator folds the composite into the existing allow/owner-scope/deny decision.
4. **External identity** — OIDC/JWKS `Authenticator`, claims→roles mapping,
   `auth.provider: oidc|both`, the reserved `jwt:`/`oidc:` config.

## Consequences

- **One extra indexed read per authenticated request** (session lookup). Constant,
  local, dwarfed by the data query; no N+1. An in-memory TTL cache keyed by token
  hash is available if profiling ever shows it, but is not built pre-emptively.
- **A real 403/404 split** means clients must authenticate for writes on the
  default policy — a behavior change from the anonymous-admin era, and the point
  of the milestone.
- **`password_hash` leakage is a serialization hazard**; it is stripped at the
  same choke point pattern as `storage_key`, and covered by a response-validation
  test.
- **The store stays untouched** — no new methods, no auth knowledge; users and
  sessions are ordinary records. This keeps ADR-0003 intact and means a future
  adapter inherits auth for free.
- **Bootstrap via env** keeps the secrets-are-env-only rule (ADR-0009) intact.

## Alternatives considered

- **Stateless JWT for local login** — rejected for this deployment model: its
  statelessness buys nothing backend-per-customer while its revocation weakness
  costs the immediacy a CMS needs (see §2). JWT still enters for external OIDC.
- **Authz in the store / row-level security in the DB** — violates ADR-0003 and
  couples policy to one adapter. The gateway is the single policy plane already
  holding lifecycle and referential rules.
- **A generic policy DSL (CEL/OPA)** — over-scoped for M1. The `access:` grammar
  covers the CMS cases (public/authenticated/roles/owner, plus `any:` OR); a
  richer expression layer can grow later without changing the enforcement location.
- **An `all:` (AND) composite alongside `any:`** — not built: no CMS case has
  needed it, and its decision-folding is less obvious (an owner-scope AND a role
  gate would have to intersect a filter with a boolean). Additive when a use case
  appears; `any:` alone keeps the grammar honest about what it supports today.
