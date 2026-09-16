# ADR-0026 — Scheduled go-lives emit a `went_live` event; core gains one time trigger, not a scheduler

Status: Accepted
Date: 2026-09-16

## Context

Publishing scheduling is passive (ADR-0012): a future `_published_at` makes a
record become visible when the clock passes it, with **no scheduler and no write**.
Events are captured in a write's transaction (ADR-0021). So a scheduled go-live —
a real state change into public visibility — produces no `_events` row: neither the
change feed (`GET /_changes`) nor webhooks report it (GitHub #28, found in
Agrojatra's static-site sync).

Crucially the change feed is *the* mechanism a consumer is told to use instead of
re-scanning the dataset, and it is structurally incomplete here: a pull consumer
polling `/_changes` misses the go-live too, because there is no write to record.
An SSG rebuild triggered at schedule time correctly omits the not-yet-live record,
and then nothing fires at go-live time — the record stays off the site until an
unrelated rebuild.

## Decision

### Boundary first: complete the feed, don't add a scheduler

The question "should scheduling be in core?" splits in two:

- **Passive scheduled publishing stays core** (already is): a future
  `_published_at` honored by the read path. Zero background process.
- **Reacting when the clock crosses that time** is the contested part. We add it to
  core, but strictly as *completing an existing core promise* (the change feed
  captures every state change), **not** as a general scheduler.

The hard line: `went_live` is the **only** time-triggered behaviour in core. Core
does not gain cron expressions, "run task at T", or arbitrary scheduled actions —
those remain the app's job (or a future opt-in plugin), triggered off the data.
This nuances, but does not discard, ADR-0012's "timestamp-driven, no scheduler"
stance: the visibility model is still passive; the only thing that fires on a timer
is the event that keeps the feed honest.

### Mechanism: a due-at outbox, so exactly-once and precisely scoped

A publish transition already writes. When it sets a **future** `_published_at`, a
marker row is enqueued in that same transaction in an engine-managed
`_scheduled_publishes` outbox (`reconcileScheduled`, in the write's Tx). A
background worker (`RunScheduledPublishes`, beside the webhook/notification pollers)
scans for markers whose `due_at` has passed and, in one transaction, emits a
`went_live` event (`from_status: scheduled`) and deletes the marker — so the emit
is **exactly-once** by construction.

- **Only scheduled crossings.** An *immediate* publish (`_published_at <= now`)
  enqueues no marker; its own `published` event is the go-live signal. Only a
  future-dated publish arms a marker, so `went_live` fires only for a genuine
  scheduled crossing — resolving the immediate-vs-scheduled ambiguity a bare
  `_published_at` cursor could not.
- **Reconcile, not per-op special-casing.** Every lifecycle write on a
  publishing+events collection clears any existing marker and re-arms it iff the
  record is now a future go-live. So publish→reschedule replaces the marker, and
  unpublish/archive/trash/hard-delete clear it, uniformly.
- **Worker re-reads before emitting.** At due time the worker re-reads the record
  and emits only if it is genuinely live now (published, reached, not trashed);
  otherwise it just consumes the marker. So a cancellation the reconcile somehow
  missed still can't produce a spurious event, and a record rescheduled further out
  is left armed.
- **Opt-in by construction.** The outbox and worker exist only when a collection
  both `publishing:` and `events:`. A schema that never uses both pays nothing.

`went_live` rows are ordinary `_events` rows, so the change feed and webhooks carry
them with no special handling; a webhook can filter on the `went_live` type.

## Consequences

- `/_changes` and webhooks are now complete: a scheduled go-live is observable, so
  an SSG rebuilds at the right moment with no site-specific polling of
  `?status=scheduled`.
- Exactly-once delivery across restarts: the marker is the durable intent, consumed
  in the same Tx as the event.
- Core has exactly one time-triggered behaviour, tightly scoped to the feed's
  correctness; it is explicitly not a general job scheduler.
- Supersedes-in-part ADR-0012's "no scheduler" note: visibility stays passive, but a
  worker now realizes the go-live *event* at its due time.
- Out of scope: general timed jobs / cron, and a `went_live` for immediate publishes
  (the `published` event already serves that). Precision is bounded by the poll
  interval (15s), which is ample for the static-build use case.
