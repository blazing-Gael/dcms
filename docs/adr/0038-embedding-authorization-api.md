# ADR-0038 — Embedding authorization API: `Can`, `As(p)`, and in-process media for custom routes and hooks

Status: Accepted
Date: 2026-10-28

## Context

ADR-0031 lets an embedder add Go hooks and custom routes. A route receives the
verified `Principal` and a store handle — but the handle is **privileged**: it skips
access rules, field masks, and lifecycle visibility. There is no public way to ask
the engine "may this principal do X to this record?", to read or write *as* that
principal, or to use derived media files and signed URLs from Go (GitHub #68).

So every custom route that touches user data re-implements authorization by hand:
an enrollment query before serving a lesson, the same query again before issuing a
playback grant. That is authz outside the rule engine — invisible to `/__schema`,
untested against the HTTP behaviour, and a fresh chance for a hole — exactly what
ADR-0036 (`exists`, `all`/`any`) set out to remove. Worse, the privileged handle is
the *default* on a route, so the easy path is the unsafe one.

The obstacle is structural. Authorization lives in the gateway and is entangled with
HTTP: each check (`authorizeCreate(w, r, …)`, `authorizeRecordWrite(w, r, …)`,
`listReadFilters(w, r, …)`) writes its own 401/403/404, and the CRUD handlers
interleave parsing, policy, the write pipeline, and response shaping. The gateway
instance is also built only inside `Serve`, so an `App` has nothing to delegate to.

## Decision

### 1. Transport-free core in the gateway

The authorization checks and the CRUD/media operations are split into functions that
take a `context.Context` (carrying the principal) and return values or typed errors;
the HTTP handlers become parse → core → respond. The core is the **single** source of
policy: the HTTP API and the Go API run the same code, so they cannot diverge.

Typed outcomes, mapped to HTTP exactly as today:

| Go error | HTTP |
|---|---|
| `ErrNotFound` (absent *or* hidden from the caller) | 404 |
| `ErrUnauthenticated` (denied, anonymous caller) | 401 |
| `ErrForbidden` (denied, or a forbidden value-scoped transition) | 403 |
| `*ValidationError` | 422 |
| `ErrVersionConflict` | 412 |
| `*HookError` | the hook's status |

### 2. One gateway per App

`internal/app` builds the gateway once — at `Serve`, or at the first API call if that
comes earlier — and `engine.Serve` uses that instance. Library calls and HTTP requests
therefore share hooks, signer, quota, and rate-limit state. Building it freezes
registration: `On`/`Route` after that point return an error instead of being silently
ignored.

### 3. Public API (`package dcms`)

```go
app.Can(ctx, p, dcms.ActionRead, "lessons", id) (bool, error)
app.ReadFilters(ctx, p, "lessons") (filters []dcms.Filter, ok bool, err error)

s := app.As(p) // ScopedStore: Get / List / Create / Update / Delete as p
app.Media().PutFile(ctx, p, assetID, path, r)                    // as PUT /__media/{id}/files/{path}
app.Media().SignedURLAs(ctx, p, assetID, scope, path, ttl)       // as POST /__media/{id}/sign
app.Media().SignedURL(assetID, scope, path, ttl)                 // system use, no check
```

- **`Can`** returns `false` (not an error) for "not allowed" and for "not found / not
  visible", matching the 404 policy; `err` is reserved for real failures. Actions:
  read, create, update, delete, publish, history.
- **`ReadFilters`** returns the read scope *and* lifecycle filters to AND onto the
  caller's own query; `ok == false` means the caller may read nothing.
- **`As(p)`** enforces everything the HTTP API does for p: read scope, lifecycle
  visibility (the default, published-only view), field masks on results, write-field
  filtering and value-scoped transitions, validation, and the full write pipeline —
  hooks, events, revisions, concurrency (`Expect` version). Filters and sort on fields
  p cannot read are rejected, as over HTTP. Engine collections (`_…`) are not
  reachable, as over HTTP.
- **Media**: `PutFile` applies the same authorization, extension allowlist, size cap,
  quota, and `derived_size` accounting as the HTTP route. `SignedURLAs` applies the
  same read check and scope validation as `/sign`; `SignedURL` is the explicit
  unchecked variant for system work (e.g. emailing a certificate link from a job).
  URLs are path-relative; prefix your public origin.

### 4. Safe by default on custom routes (breaking)

`Request.Store` becomes a `ScopedStore` acting as `req.Principal`. The privileged
handle moves to `Request.System`. `req.Can(...)` is a shortcut. Hooks are unchanged:
`hc.Store` stays privileged and transaction-enrolled — hooks are system code reacting
to an already-authorized write.

DCMS is 0.x; the in-tree callers are the docs and example, updated in the same change.

## Consequences

- One authorization implementation for HTTP and Go; parity tests pin the two together
  for every rule kind (public, roles, owner, owner_field, exists, all/any, preview,
  history, field masks, value-scoped transitions, lifecycle).
- Custom routes get the rule engine for free; edtech's lesson and playback routes need
  no hand-written enrollment queries.
- Embedders who relied on `req.Store` being privileged must switch to `req.System`.

## Not now

- `As(p)` inside a hook's transaction (hooks keep the privileged `hc.Store`).
- Lifecycle transitions (publish/unpublish/restore) and aggregates through `As(p)`.
- Non-default visibility views (`?status=`, preview token) through `As(p)`.
- RPC/WASM transports (ADR-0031/0032) — this API is their eventual in-process model.
