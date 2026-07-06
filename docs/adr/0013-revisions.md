# ADR-0013 — Revisions: append-only version history

**Status**: Accepted

## Context

Editorial content needs history: who changed what, when, and the ability to roll
back. It is the natural companion to the publishing lifecycle (ADR-0012) — an
editor who can publish also expects to undo. Constraints carried forward:

- The `store` interface is **locked** (ADR-0003): history must ride existing
  methods, not new ones.
- **Performance is non-negotiable**: capture must add no N+1 and no read-path
  cost; a bounded per-write cost on opted-in collections is acceptable.
- Policy lives at the **gateway**, not the store (as with ADR-0010/0012).

## Decision

Opt-in per collection with `revisions: true`. History lives in one
engine-managed, reserved `_revisions` collection (injected only when at least one
collection opts in), so it rides the locked store as an ordinary table:

```
_revisions: id, collection, record_id, version, operation, data(json)
            + audit columns (created_at = when, created_by = who)
```

- **Full snapshot per version.** `data` holds the entire record JSON at that
  version. Restore is trivial (write it back); diffs are computed on read.
  Content rows are small, storage is cheap, and a retention cap can bound growth
  later. (Delta/patch was rejected as complexity unjustified for KB-sized rows.)
- **Captured synchronously, inside the write's transaction.** After a successful
  create / update / lifecycle transition / soft-delete, the gateway inserts the
  revision row on the same `tx`, so history can never diverge from the record. It
  is one extra `INSERT` (plus an indexed max-version lookup) per mutation, only on
  revisioned collections — no N+1, no read cost. Writes are far rarer than reads
  in a CMS, so this is the right trade over an eventually-consistent async path
  that could momentarily lag or lose history on a crash.
- **`version`** is a per-record monotonic counter (unique on
  `(collection, record_id, version)`); v1 is the create.
- **`operation`** labels the mutation (`create`, `update`, `publish`, `unpublish`,
  `archive`, `restore`, `delete`) so history reads like a timeline.

### Endpoints

- `GET  /{collection}/{id}/revisions` — the history (metadata only; the heavy
  snapshot is omitted from the list).
- `GET  /{collection}/{id}/revisions/{version}` — one version, with its snapshot.
- `POST /{collection}/{id}/revisions/{version}/restore` — roll back.

**Restore is content-only and append-only.** It writes the snapshot's *content*
fields back through the normal update path (which strips managed columns), so
`_status`/`_published_at`/`_deleted_at` are left as they are now — restoring old
text to a live article does not surprise-unpublish it — and the restore itself is
recorded as a new revision. History is never rewritten.

### Access

Reading history is privileged (a revision may hold unpublished content), so when a
preview token is configured (ADR-0012) revision reads require it; otherwise they
are open, consistent with the pre-auth posture of the rest of the API. Auth will
gate all of this by role later.

## Consequences

- Exactly one extra indexed lookup + insert per mutation on revisioned
  collections; nothing on others, nothing on reads.
- Purge (hard delete) does not capture a revision and leaves prior revisions in
  place as an orphaned audit trail (unreachable via the record endpoint). GC of
  purged-record revisions is a later refinement.

## Deferred

- **Many-to-many links in snapshots**: v1 snapshots the base row (scalars,
  belongs-to, managed, audit); m2m link sets are not captured/restored yet.
- **Nested inline children**: only the top-level written record is versioned.
- **Retention caps** (`keep: N`), server-side **diffing** endpoints, and
  purged-record revision GC.
