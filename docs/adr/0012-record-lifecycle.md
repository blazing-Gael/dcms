# ADR-0012 — Record lifecycle: publishing states + soft-delete

**Status**: Accepted

## Context

A typed CRUD API becomes a *content management system* when content gains a
lifecycle. Editors expect to work on a **draft** before it is visible, to
**schedule** a go-live, to **retire** content without losing it, and to delete
into a **trash** they can restore from. An ecommerce operator needs the same
things: a product staged before launch, a sale scheduled for a date, a
discontinued SKU kept for order history, an accidental delete undone.

Two features cover this, and they share almost all their machinery — managed
state, a default visibility filter, transition actions, and an admin bypass:

1. **Publishing** — draft / published / scheduled / archived.
2. **Soft-delete** — delete marks a row as trashed; restore brings it back;
   purge removes it for good.

Constraints from earlier decisions:
- The `store` interface is **locked** (ADR-0003): no new methods. Additive `Op`
  constants and data fields are allowed; policy that needs schema awareness lives
  at the gateway, never in the store (as with referential integrity, ADR-0010).
- The schema is the **single source of truth** (ADR-0001): opting a collection
  into a lifecycle must derive its columns, routes, filters, OpenAPI, and SDK.
- **Performance is non-negotiable** (no N+1, no read regressions): the visibility
  filter must be a cheap, indexed predicate and must not add per-row queries,
  even when applied through relation expansion.
- **Auth does not exist yet.** A secure admin/preview bypass ultimately needs it;
  the interim gate must not pretend otherwise.

## Decision

### Opt-in per collection, engine-managed columns

Two collection directives, off by default:

```yaml
products:
  publishing: true     # adds _status + _published_at
  soft_delete: true     # adds _deleted_at
```

They add reserved, `_`-prefixed, engine-managed columns — the same philosophy as
audit columns. Users cannot declare them; clients cannot set them through normal
create/update (managed keys are stripped from request bodies); they are always
readable.

- **`_status`** — `draft` (DDL default) · `published` · `archived`.
- **`_published_at`** — nullable datetime; the go-live instant.
- **`_deleted_at`** — nullable datetime; the trash marker.

Each enabled column is indexed, because it appears in the default read predicate.

### Publishing is timestamp-driven — scheduling needs no scheduler

The stored state is minimal; "scheduled" is *derived*, not stored:

```
public-visible  ⟺  _status = 'published'  AND  _published_at <= now()
```

- `_published_at` in the **future** ⇒ the row is published but not yet visible —
  it becomes visible the instant the clock passes it, purely at read time, with
  **no background job**. (`_published_at` and `now()` are both RFC3339 UTC text,
  which sorts lexicographically, so the comparison is a plain indexed `<=`.)
- `archived` is visible to neither the public nor a "drafts" view — it is
  retired-but-kept, distinct from a draft, so order history and references keep
  working.

Transitions are **explicit action endpoints** (auditable, server-controlled — not
clients poking managed columns):

- `POST /{collection}/{id}/publish` `{ "at"?: "<RFC3339>" }` → `published`,
  `_published_at = at || now` (a future `at` schedules it).
- `POST /{collection}/{id}/unpublish` → `draft`, `_published_at = null`.
- `POST /{collection}/{id}/archive` → `archived`.

### Soft-delete / restore / purge

- `DELETE /{collection}/{id}` on a soft-delete collection sets `_deleted_at = now`.
- `POST /{collection}/{id}/restore` clears it.
- `DELETE /{collection}/{id}?purge=true` hard-deletes (and still honors the
  `on_delete` referential rules — see the interaction note below).

### Default filtering, applied everywhere

The gateway injects the visibility predicate — `_status='published' AND
_published_at<=now` and/or `_deleted_at IS NULL` — into every public read of an
enabled collection: lists, get-one (a hidden record answers **404**, never 403,
so existence does not leak), **and relation expansion**. Expansion stays batched:
the same predicate is added to the batched `IN`/inverse queries, so no N+1 is
introduced. A belongs-to pointing at a hidden record keeps the **id** (the
relationship is intact) but is not inlined.

### The admin/preview bypass — a token until auth lands

A secret **`DCMS_PREVIEW_TOKEN`** (env-only) gates the bypass. A request carrying
`X-DCMS-Preview: <token>` may pass `?status=draft|published|archived|scheduled|any`
and `?include_deleted=true|only`; without the token these params are **ignored**
(the public sees only live, non-trashed content). The token compare is
constant-time. When auth arrives, this upgrades to role-based preview without a
client-visible change to the default behavior.

### Respecting the locked store

Same split as referential integrity: **policy at the gateway**, generic store.
- Read filtering = injected `store.Filter`s.
- Transitions, soft-delete, restore = server-built `Update`s (a `nil` value writes
  `NULL`, so restore is just `_deleted_at = nil`).
- The one additive store change: **`IsNull` / `NotNull` filter operators**
  (new `Op` constants — additive, not a method change — and broadly useful for any
  nullable field). Adapters emit `col IS [NOT] NULL`.

## Consequences

- Scheduling and expiry-style visibility need no scheduler — a huge simplification
  — at the cost of "fire an action exactly at go-live" (a webhook the moment a post
  publishes), which needs a real scheduler and is deferred to events/webhooks.
- **Soft-delete bypasses `on_delete: restrict`**: the row still exists, so there is
  no FK conflict — a trashed-but-referenced record simply disappears from public
  reads (reversible). **Purge** honors `restrict` → `409`. This is the intended
  split; it is documented so it is not a surprise.
- Publishing is **per-record and independent of relations** in v1: a published
  record may reference an unpublished one (which just won't expand publicly). A
  "cannot publish while referencing unpublished content" guard is a deferrable
  editorial nicety.
- The preview token is a shared secret suited to a single-tenant backend; it is
  explicitly an interim measure, replaced/augmented by auth.

## Alternatives considered

- **Store-driven soft-delete** (the store inspects its own `_deleted_at` column):
  rejected — it spreads policy into the generic layer and fights the locked-store
  discipline that keeps referential integrity at the gateway.
- **Pure `_published_at` model with no `_status`** (Strapi-style): simpler, but
  cannot distinguish "never published draft" from "retired/archived", which the
  operator use cases need. We keep the enum.
- **A background scheduler flipping a boolean at go-live**: rejected for the common
  case — read-time comparison is simpler and correct; a scheduler is reserved for
  actual side effects.

## Deferred

Expiry (`_unpublish_at`), publish-time side effects (with events/webhooks), a
"block publish if refs unpublished" guard, and revisions/version history (a
separate follow-on that builds on these states).
