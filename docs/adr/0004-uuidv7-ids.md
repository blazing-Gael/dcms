# ADR-0004: UUIDv7 ids by default, via an injectable generator

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

Record primary keys must be stable, portable, safe to expose, and performant as
a database index. Random UUIDv4 has poor index locality (random inserts scatter
the B-tree). Auto-increment integers are not portable or safe to expose.

## Decision

Default to **UUIDv7** (time-ordered) ids, generated in Go via an **injectable
`store.IDGenerator`** (`store.UUIDv7` default, `store.UUIDv4` provided, or a
custom function via adapter `Config`). Client-supplied ids are respected when
present.

## Consequences

- Near-sequential inserts → good index locality and write performance; ids sort
  chronologically, so the default `id` ordering is chronological for free.
- Per [ADR-0002](./0002-configurable-except-essentials.md), the strategy is not
  forced — switch to v4 (or custom) when needed.
- **Trade-off:** a v7 id embeds its creation timestamp. For data where that must
  not leak, use v4 for that collection.

## Alternatives considered

- **UUIDv4 default**: worse index locality; no benefit here.
- **DB-generated ids** (e.g. `gen_random_uuid()`): not portable across adapters;
  generating in Go gives identical behavior everywhere.
