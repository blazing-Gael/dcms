# ADR-0027 — Optimistic concurrency via a `_version` counter and `If-Match`

Status: Accepted
Date: 2026-09-16

## Context

Writes had no concurrency control (GitHub #26, found in Agrojatra where a writer
and an editor edit the same story). Two concurrent `PATCH`es both read version N,
both write, and the second silently overwrites the first — a lost update with no
signal. Content ETags existed only on `GET` responses (a content hash for
`If-None-Match` caching); nothing tied a write to the version it was based on.

## Decision

Add an opt-in `concurrency: true` collection directive that provides
**optimistic concurrency** through an engine-managed `_version` counter and the
standard HTTP `If-Match` precondition.

- **`_version`** is a managed integer column (like the lifecycle columns): reserved,
  readonly, never client-settable. It is `NOT NULL DEFAULT 1`, so a create — and
  every pre-existing row on migration — starts at 1 with no gateway involvement. The
  gateway increments it on every write.
- **`If-Match`** carries the version the client last saw (`If-Match: "3"`, bare or
  quoted). Inside the write's transaction the gateway reads the current version,
  and if it no longer matches returns **412 Precondition Failed**
  (`VERSION_CONFLICT`); otherwise it proceeds and bumps `_version`. Read, check, and
  increment run in one transaction, so under the store's single writer the
  check-and-set is atomic.
- **Per-request opt-in.** A write *without* `If-Match` still succeeds
  (last-writer-wins), so concurrency is enforced only when a client asks. This keeps
  existing clients working while letting a careful one protect itself.
- **No silent no-op.** Sending `If-Match` to a collection that does not enable
  `concurrency` is a **400**, not a silently-ignored header — the same "a typo/misuse
  must fail loudly" stance as ADR-0025, so a client never believes it is protected
  when it is not.

It applies to every write path — `PATCH` (plain, inline-relations, and m2m-linked),
the lifecycle transitions, `restore`, and `delete` — via two small helpers
(`applyVersion` for updates, `checkVersion` for deletes) invoked inside each write's
existing transaction. A versioned collection is forced onto the transactional write
path (`needsWriteTx`) so the check is always atomic.

### Why a dedicated counter, not `updated_at`

`updated_at` is a timestamp with second/sub-second resolution and is set by the
adapter; two writes in the same tick could share it, and comparing timestamps as a
concurrency token is fragile across clocks and adapters. A monotonic integer is an
unambiguous token, is trivially comparable, and doubles as a human-readable revision
number. It also composes cleanly with a future retention policy for revisions
(issue #25), which can key off the same counter.

### Why gateway-owned, not a store primitive

The store interface stays locked (ADR-0003). `_version` is a normal column the
gateway sets (like `_status`/`_published_at`), and the compare-and-increment lives
in the gateway's write transaction — no new store method, no `WriteInput` field. The
adapter remains authz- and policy-agnostic.

### Response shape

`_version` rides in every response body (it is a column), so a client always has the
token to send back. The existing content-hash `ETag` on `GET`s is left unchanged;
`If-Match` carries the version number directly, keeping the two concerns separate.

## Consequences

- A client that sends `If-Match` can never lose an update to a concurrent write; it
  gets a 412 and re-reads, exactly the multi-editor case #26 describes.
- Opt-in per collection *and* per request: unversioned collections and no-`If-Match`
  writes behave exactly as before, so nothing existing breaks.
- One extra in-transaction read per versioned write (to fetch the current version);
  versioned collections always take the transactional write path.
- Migration is additive and idempotent (`_version INTEGER NOT NULL DEFAULT 1`), so
  `dcms serve` does not see perpetual pending migrations.
- Out of scope: a global always-on version (kept opt-in to avoid migrating every
  table and changing every collection's shape), and `If-Match`/version ETags on
  reads (the body's `_version` is the token). Revision retention (#25) is a separate
  change that can reuse this counter.
