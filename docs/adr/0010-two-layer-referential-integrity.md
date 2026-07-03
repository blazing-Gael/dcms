# ADR-0010 — Two-layer referential integrity

**Status**: Accepted

## Context

Relations (ADR builds on the schema-is-source-of-truth model of ADR-0001) let a
record point at another: a belongs-to field stores a target id in a string FK
column, and a many-to-many field is backed by an engine-managed join table. Once
records reference each other, one question becomes non-negotiable: **can the
database ever hold a reference that points at nothing?**

There are two obvious ways to prevent it, and they pull in opposite directions:

- **Application-level checks** — before a write, verify each referenced id
  exists. Portable across every adapter (SQLite, Postgres, Couchbase), and it
  can return a clean, field-named `422 {"author": "no such users record"}`. But
  it only guards writes that flow through the gateway; a bulk import, an admin
  running SQL, a second service sharing the DB, or a bug in our own code can
  still create a dangling reference. It also has a check-then-insert window under
  concurrency.

- **Database foreign keys** — the engine refuses to store a dangling reference,
  full stop, atomically, no matter who writes. But FK violations surface as
  generic driver errors (`FOREIGN KEY constraint failed`, with no field name),
  FK behavior differs across adapters (Couchbase has none), and on SQLite the
  additive migrator can't retrofit or change an FK on an existing table without a
  full table rebuild.

Picking one means giving up either the safety of the other. The performance
mandate (no N+1, no regressions after migrating to DCMS) is a separate concern —
neither mechanism affects read performance; that is solved by batched expansion.

## Decision

Treat integrity as **two distinct layers with two distinct jobs**, not one
mechanism to be chosen.

1. **Validation layer — at the gateway, always on, every adapter.**
   Before a create/update, the referenced ids of every belongs-to and
   many-to-many field are checked for existence with a *batched* query per
   target collection (one `id IN (…)` per collection, never per row). Anything
   that does not resolve is rejected with a field-named `422`. This layer owns
   *user-facing errors and early rejection*. It is uniform across all backends.

2. **Protection layer — at the store, where the adapter supports it.**
   DDL emits real foreign keys at `CREATE TABLE` time: belongs-to columns get a
   `REFERENCES target(id)` clause, and join-table `source_id`/`target_id` get
   `REFERENCES … ON DELETE CASCADE` so orphaned link rows are cleaned up for
   free. This layer owns *the invariant that the data can never reach a corrupt
   state*, including against writes that bypass the gateway. Where an adapter has
   no FKs, or where SQLite's additive migrator cannot add one to an existing
   table, the protection layer degrades gracefully — the validation layer is
   still present, so correctness for gateway writes is unchanged; only the
   backstop against out-of-band writes is weaker.

### The invariant

> **Application validation must never rely on database FK violations for
> correctness or user-facing errors.**

Concretely:

- Every reference is validated *before* the write; the `422` a client sees is
  produced by the validation layer and names the offending field. A user must
  never receive an error that is merely a raw DB constraint failure surfaced as
  the normal rejection path.
- A DB FK violation on a **create/update** is therefore an *invariant breach* —
  it means something bypassed validation. It maps to a logged, alertable `500`
  (`store.ErrIntegrity`), never a friendly `422`, because pretending it was
  ordinary validation would hide a real bug.
- A DB FK violation on a **delete** is the `RESTRICT` default doing its job (the
  parent is still referenced). It maps to a user-facing `409`, not a `500`.
  Configurable `on_delete` policies (`cascade`/`set null`) are a later
  increment; `restrict` falls out of the DB FK for free and is the right
  default.

In short: **validation is the door people use; protection is the lock that
proves the door was the only way in.** If the lock ever trips on a write, that
is a bug to fix, not a message to show.

## Consequences

**Good**
- Great DX: clean, field-named `422`s on every adapter, regardless of whether
  the DB happens to enforce the constraint.
- Real safety for the scenarios app-checks can't cover: out-of-band writes,
  concurrency/TOCTOU, crash consistency.
- Free join-row cleanup via `ON DELETE CASCADE` on join tables.
- Each layer has one reason to change; the protection layer can strengthen over
  time (e.g. when a table-rebuild migrator lands) without touching validation.
- No read-performance cost from either layer; expansion batching is orthogonal.

**Costs / limits**
- On SQLite, FKs are emitted at `CREATE TABLE`. A relation *retrofitted* onto an
  existing populated table only gets DB protection if it is nullable (SQLite's
  `ADD COLUMN` accepts `REFERENCES` only with a NULL default); otherwise it
  relies on the validation layer until a table-rebuild migrator exists. Users
  don't observe this except in the rare out-of-band-write case.
- SQLite tolerates forward references (a child table may reference a parent
  declared later), so no topological ordering of `CREATE TABLE` is required.
  Adapters that don't (e.g. strict Postgres ordering) will handle ordering or
  deferred constraints in their own adapter code.
- `foreign_keys` must be enabled on **every** pooled connection, not just one;
  the SQLite adapter sets it via the DSN so it applies pool-wide.
- FK metadata now rides on `store.ColumnMeta` (`References`, `OnDelete`) — an
  additive change to the locked store types (ADR-0003), not a new method.
- Two enforcement paths (DB vs app) mean two code paths to keep in sync; the
  invariant above keeps their *observable* behavior aligned (validation is the
  only path that produces user-facing ref errors).

## Related

- ADR-0001 — schema is the single source of truth (relations derive from it).
- ADR-0003 — locked, auth-agnostic store interface (FK metadata is additive).
- ADR-0008 — contracts derive from the schema.
