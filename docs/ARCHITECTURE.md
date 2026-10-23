# DCMS Architecture

A map of how DCMS is put together, for reviewers and contributors. For *why* a
specific decision was made, follow the ADR links to [`docs/adr/`](./adr/). This
describes what is actually built (as of `v0.1.0-beta.4`), not the roadmap.

## The one idea

**The schema is the single source of truth, and the engine owns everything
dangerous to get wrong.**

You describe a backend in `schema.yaml`. From that one file DCMS derives the
database tables, the REST API, the OpenAPI spec, validation, access rules, the
admin panel, and the typed TypeScript client — nothing is hand-written twice, so
nothing can drift. The parts that are easy to get subtly wrong — money,
concurrency, atomicity, idempotency, access enforcement — live *in the engine*,
behind declarative schema switches. So whether a human or an agent authors the
schema, the result is correct **by construction**: it declares intent, the engine
guarantees the behavior. The only code you write is the logic unique to your app,
as a small typed Go **hook**.

## Request lifecycle

Every API request flows through the same pipeline. Authorization is enforced once,
at the gateway — collection handlers assume the caller is already authorized.

```
HTTP request
   │
   ▼
 chi router ──────────── middleware: CORS · body cap (413) · timeout (504) · rate limit (429)
   │
   ▼
 authn ................. opaque DB session cookie / Bearer PAT  →  Principal
   │
   ▼
 virtual handler ....... generated per collection from the schema IR (no per-route code)
   │
   ├─ authz ............ collection rule → field read/write masks → value-scoped
   │                     write rules (legal enum transitions). ADR-0016, #27.
   ├─ validate ......... types, required, enums, refs — from the schema IR
   ├─ lifecycle ........ draft/publish/soft-delete filtering; preview-token bypass. ADR-0012
   │
   ▼
 write transaction (store.Tx)
   │   ├─ concurrency check (If-Match vs _version → 412). ADR-0027
   │   ├─ idempotency reserve (Idempotency-Key). ADR-0018
   │   ├─ BeforeCreate/Update/Delete hook ◀── your Go code runs INSIDE the Tx
   │   ├─ the write + audit stamping (created_by, timestamps) + revision snapshot
   │   ├─ events appended to the _events outbox (same Tx). ADR-0021
   │   └─ AfterCreate/Update/Delete hook
   │
   ▼
 response envelope  {data, meta:{limit,total,next_cursor}}
```

The hook running *inside* the write transaction is the crux: a hook that checks
stock and the order that decrements it either both commit or both roll back. There
is no window where one happened without the other.

## Package map

The public surface is **the binary, the HTTP API, and the `dcms` Go package** —
not the internals. Everything under `internal/` is free to change without a major
version bump; the Go compiler enforces that outside code can't import it.

**Public (stable-ish):**

| Path | What it is |
|---|---|
| `./` (`dcms.go`, `dcms_routes.go`) | The public **`dcms` package**: the `App` builder (`New` / `On` / `Route` / `Serve`), the hook API (`HookContext`, `HookStore`, `HookError`), and re-exported query types. How you embed DCMS or attach business logic. See [`EMBEDDING.md`](./EMBEDDING.md), [`HOOKS.md`](./HOOKS.md). ADR-0031. |
| `pkg/auth` | The external-identity seam: `Authenticator`, `Principal`, `Claims`. Bring-your-own auth works out-of-tree. ADR-0020. |
| `cmd/dcms` | The CLI: `dev`, `serve`, `migrate`, `validate`, `codegen`, `token`, `init`. |

**Internal (the engine):**

| Package | Responsibility |
|---|---|
| `internal/schema` | Parses YAML/JSON → the **IR** (`CollectionDef` + `FieldDef`). Strict keys: an unknown key is a compile error, not a silent ignore (ADR-0025). This IR is what everything else reads. |
| `internal/store` | The storage contract (`Store`, `Tx`) plus the SQLite adapter (`modernc.org/sqlite` — **pure Go, no CGO**, so the binary is static and cross-compiles). UUIDv7 ids, audit columns. The interface is the swap point for Postgres. See [`STORE_INTERFACE.md`](./STORE_INTERFACE.md). |
| `internal/gateway` | The heart (~50 files). Builds a virtual HTTP router from the IR and owns authn, authz (collection/field/value-scoped), validation, lifecycle, concurrency, idempotency, media endpoints + quotas, events/webhooks/change-feed, the admin panel, and hook dispatch. |
| `internal/engine` | Composition: load the schema, run migrations, wire the gateway, serve. |
| `internal/codegen` | Schema IR → TypeScript types (`dcms codegen`). |
| `internal/config` | Layered settings: flags > env > config file > defaults. Secrets are **env-only** (no file interpolation yet). ADR-0009. |
| `internal/blob` | `blob.Store` for media bytes (filesystem backend today); the `_media` collection in the gateway references it. ADR-0011/0028. |
| `internal/app` | Bridges the public `App` options to the internal engine/gateway. |

## Invariants a contributor must respect

These are load-bearing. Breaking one causes silent, hard-to-debug corruption, so
reviewers should guard them:

1. **The store trusts the gateway.** Access rules, validation, and field masks are
   enforced in `internal/gateway` *before* the store is called. The store does no
   authorization. Never move an auth check into the store, and never call the store
   directly from a path that bypasses the gateway's checks. (Hooks get a narrow
   `HookStore` that preserves this.)
2. **Every write goes through a `Tx`.** Order + inventory, record + revision, record
   + event outbox — these must be atomic. If you add a write path, it participates
   in the existing transaction; it does not open its own.
3. **Nothing derives data from anything but the schema IR.** OpenAPI, TS types,
   admin widgets, and validation all read the parsed IR. Never hardcode a field
   name or a type in generated output — that reintroduces drift.
4. **The response never waits on background work.** Webhook delivery and other
   async effects fire from the `_events` outbox after commit; the client gets its
   response immediately.
5. **Audit fields are engine-owned.** `created_at/updated_at/created_by/updated_by`
   are stamped by the adapter from the request principal, never accepted from the
   client body.

## What's built vs. planned

Built and exercised by [`examples/shop`](../examples/shop): schema→REST+OpenAPI,
auth (sessions + PATs), collection/field/value-scoped access, lifecycle, exact
money, optimistic concurrency, idempotency, media + quotas, events + signed
webhooks + change feed, revisions, Go hooks + custom routes, the admin panel, and
TypeScript codegen.

Planned (seams exist, implementation deferred): the **Postgres adapter** (the store
interface is the swap point), **Wasm plugins** (ADR-0032/0033, design only — hooks
are Go-only today), **semantic search / embeddings**, a **full non-TS SDK**, and
**generic OIDC**. See [`ROADMAP.md`](../ROADMAP.md) and `TODO.md`.
