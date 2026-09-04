# ADR-0021 — Events, change feed, and signed webhooks (M-B)

Status: Accepted
Date: 2026-09-04

## Context

DCMS is on the read path for content but has no way to tell the outside world
when content changes. The one place this hurts is the publish→rebuild path: a
static-site generator or an ISR frontend running against DCMS can only discover a
new post by polling `updated_at`, which forces a choice between a slow publish
(long poll interval) or a hot loop against the API. This is requested in GitHub
issue #2, with a concrete consumer-side contract, and it is the M-B milestone on
the production roadmap.

Two earlier ADRs point here:

- ADR-0012 (record lifecycle) deferred "fire an action exactly at go-live" to
  events/webhooks — the interesting signals are the lifecycle *transitions*
  (`published`, `unpublished`, `archived`, `restored`, `soft_deleted`,
  `restored`), not raw CRUD. A `create` on a draft is noise; a draft→published
  transition is the whole point.
- ADR-0019 built the `Notifier` seam for account email and noted it should later
  re-route through "an outbox-backed impl" so delivery is durable and retried.

There is also existing precedent for the mechanism: revisions (ADR-0013) capture a
snapshot **in the same transaction** as the write (`captureRevision`), and the
embedding pipeline is designed around an outbox table drained by a background
worker. M-B is the same shape applied to change notification.

Constraints this must respect:

- **The store interface is locked (ADR-0003).** No new store methods. Events are
  captured with the existing `Create` inside the existing `Tx`, and drained with
  the existing `Find`/`RawExec`. No conversion or delivery logic in the store.
- **Never block the response / no N+1 (the standing performance mandate).** Event
  capture is one extra INSERT inside a write that already opened a transaction
  (the same cost model as revisions, and opt-in per collection). All delivery is
  asynchronous on a background worker, never on the request path.
- **Configurable except essentials (ADR-0002).** Webhook endpoints and secrets
  are opt-in config; secrets are env-only (config layering).
- **Verified-actor attribution (ADR-0005/0016).** An event records who triggered
  it from the verified principal, never from client input.

## Decision

Introduce a single durable **event log** (an outbox) written transactionally with
each state change, and expose it two ways: a **change feed** for pull consumers
and **signed webhooks** for push consumers. Webhooks are a latency optimisation
layered on a correct feed — not a separate source of truth.

### 1. `_events` — the append-only log, captured in the write transaction

A new engine-managed collection `_events` (leading underscore, not JSON-routable),
injected after the other engine collections. One row is appended per state change,
**inside the same `Tx`** as the write, mirroring `captureRevision`:

| field | meaning |
|---|---|
| `id` | UUIDv7 (time-ordered id, ADR-0004) — also the change-feed cursor (§3) |
| `collection` | the affected collection |
| `record_id` | the affected record |
| `event` | `created` / `updated` / `deleted` / `published` / `unpublished` / `archived` / `restored` / `soft_deleted` |
| `from_status`, `to_status` | lifecycle transition endpoints (null for non-lifecycle events) |
| `occurred_at` | RFC3339 UTC |
| `actor` | verified principal id that caused it (nullable for anonymous) |

Deliberately **no record body** — the payload is a pointer, and the consumer
re-fetches through the normal API. This keeps rows small, avoids leaking a draft
body to a webhook endpoint, and sidesteps field-level read-masking on the wire
entirely (issue #2, point 5).

Capture is **opt-in per collection** via a schema directive (`events: true`),
exactly like `revisions:` and `publishing:`. A collection that does not declare it
writes no event rows and pays nothing — the blog enables it on `posts`, an
internal lookup table leaves it off. Emission points are the existing write and
transition handlers (`createRecord`/`updateRecord`, delete, and
`handlePublish`/`Unpublish`/`Archive`/`Restore`).

### 2. `_event_deliveries` — per-endpoint delivery state (webhooks only)

Because one event can fan out to several webhook endpoints, delivery state lives in
a second engine collection, `_event_deliveries`, one row per (event, endpoint):
`event_id`, `endpoint` (name), `status` (`pending`/`delivered`/`failed`/`dead`),
`attempts`, `next_attempt_at`, `last_error`, `delivered_at`. The `_events` log
stays immutable; deliveries are the mutable retry ledger. The change feed reads
`_events` and ignores deliveries entirely.

### 3. Change feed: `GET {base}/_changes?since=<cursor>`

A keyset-paginated read of `_events` ordered by the time-ordered `id`:
`WHERE id > :cursor ORDER BY id LIMIT n`, returning `{ data: [event…], next_cursor }`
where `next_cursor` is the last row's `id`. This is issue #2's "cheap change feed":
a correct poller is **O(changes)**, not O(records), and needs no delivery
infrastructure. It is admin/authenticated (events reveal ids + transitions). The
cursor keys on the primary-key index, so paging is efficient and portable to
Postgres unchanged.

**Cursor correctness.** A change-feed cursor must be *commit-ordered* and
monotonic, or a low-id event that commits after the poller advanced would be
skipped. Two facts make the UUIDv7 `id` a safe cursor **today**: `google/uuid`'s
v7 generator is monotonic within the process (mutex-serialised, with a sub-
millisecond sequence), and the SQLite adapter serialises writes through a single
reserved connection — so id order equals generation order equals commit order.
**This holds because of SQLite's single writer and must be revisited for the
Postgres adapter (M-E)**, where concurrent writers can commit out of id order and
reintroduce the outbox visibility gap (handled the standard way: a bounded read
lag, or a commit-assigned sequence). Choosing the existing `id` over an invented
autoincrement keeps the log portable and adds no column or index.

### 4. Webhooks: signed, retried, dead-lettered

A bounded background worker — an extension of the existing `RunMaintenance` loop —
drains `pending` deliveries whose `next_attempt_at` has passed, POSTs to the
endpoint, and records the outcome. Delivery is **never** on the request path.

Payload is the minimal event object. The signature covers the **raw body**, so a
receiver verifies before parsing:

```
POST <endpoint>
X-DCMS-Event:     records.published
X-DCMS-Delivery:  <event id>                 # stable across retries → dedup key
X-DCMS-Timestamp: <unix seconds>             # inside the signed material (anti-replay)
X-DCMS-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + body)>
```

- **HMAC-SHA256** over `timestamp + "." + raw_body` with a per-endpoint secret
  (env-only). Including the timestamp in the signed material is what stops replay;
  the receiver rejects a skew beyond a few minutes.
- **`X-DCMS-Delivery` is the event id**, stable across every retry, so a receiver
  dedupes idempotently — reusing the idempotency primitive from ADR-0018.
- **Retries** with exponential backoff up to a configured max, then the delivery
  is marked `dead` (dead-letter). A non-2xx or a timeout is a failure.

Recovery, because webhooks get missed: `POST {base}/_events/replay?since=<ts>`
(admin) re-enqueues deliveries for events since a timestamp, and the dead-letter
set is listable. The change feed is itself the zero-infrastructure resync path.

### 5. Config (opt-in, secrets env-only)

```yaml
events:
  change_feed: true          # expose GET /_changes (default: on when any
                             # collection declares events:)
  webhooks:
    - name: site-rebuild
      url: https://hooks.example/dcms
      secret_env: DCMS_WEBHOOK_SITE_REBUILD_SECRET   # HMAC key, env-only
      events: [published, unpublished, deleted]      # filter; default all
      collections: [posts, pages]                    # filter; default all
      max_attempts: 12
```

No webhooks configured ⇒ no delivery worker cost; the change feed still works if
any collection captures events.

### 6. The `Notifier` folds into the outbox (later phase)

Account email (ADR-0019) becomes another producer of the same outbox with a
different deliverer: an outbox-backed `Notifier` writes a `kind: email` row and the
worker delivers it via SMTP with the same retry/dead-letter machinery. This
unifies "notification" and "event" delivery behind one durable mechanism and
retires the fire-and-forget email path. Scoped to a later phase so it doesn't gate
webhooks.

## Phasing

1. **Phase 1 — event log + change feed.** `_events`, transactional capture,
   `events:` schema directive, `GET /_changes`. Delivers issue #2's correct poller
   with zero delivery infrastructure and no new dependencies.
2. **Phase 2 — signed webhooks.** `_event_deliveries`, the delivery worker, HMAC
   signing, retries/backoff/dead-letter, endpoint config, `/_events/replay`.
3. **Phase 3 — unify the `Notifier`** onto the outbox (durable, retried email).

## Consequences

- Frontends move from polling `updated_at` (O(records), a latency/^load tradeoff)
  to an O(changes) feed or push webhooks — the publish→rebuild path issue #2 asked
  for.
- The store stays locked: capture is `Create` in `Tx`; the worker uses
  `Find`/`RawExec`. No ADR-0003 change.
- Capture cost is one INSERT per write on collections that opt in — the accepted
  revisions cost model. Delivery is fully async; no request-path or N+1 impact.
- Events are an immutable audit-grade log of state changes, useful beyond webhooks
  (analytics, sync, the resync path).
- One deliberate limitation carried forward: the gap-free `seq` cursor relies on
  SQLite's single-writer serialisation and **must be revisited for Postgres**.

## Alternatives considered

- **Deliver straight from the request (no outbox).** Rejected: couples the write's
  latency and success to a remote endpoint, loses events on crash, and violates
  the never-block mandate.
- **Emit raw CRUD only.** Rejected: a draft `create` is noise and a
  draft→published transition — the signal a publishing workflow cares about — would
  be invisible. Events carry the transition with `from`/`to`.
- **Put the record body in the payload.** Rejected: larger rows, leaks draft
  content to endpoints, and reintroduces field-level read-masking on the wire. A
  pointer + re-fetch is smaller and safe.
- **Webhooks without a change feed.** Rejected: webhooks get missed, and "redeploy
  to recover" is not a recovery path. The feed makes the poller correct on its own;
  webhooks are the latency optimisation on top.
