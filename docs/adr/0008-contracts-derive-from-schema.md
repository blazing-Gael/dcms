# ADR-0008: Validators, OpenAPI, and SDKs all derive from the schema

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

Type safety and validation only hold if the server's runtime checks, the
published contract, and generated client types agree. Maintaining them
separately guarantees drift.

## Decision

All contract artifacts derive from the parsed schema's field constraints
(`required`, `type`, `min`/`max`, `pattern`, enum `values`):

- **Request validators** (runtime, server-side) → 422 with field-level messages.
- **JSON Schema per collection** → the common currency.
- **OpenAPI 3.1** (`/__openapi`), **docs** (`/__docs`), **response validators**,
  and **SDKs** are all built from that JSON Schema / IR.

**Versioned contracts:** (a) URL API version (`/api/v1`) for breaking changes,
and (b) a **contract hash** — a stable hash of the canonical schema — stamped
into `/__schema`, OpenAPI `info.version`, an `ETag`, and generated SDKs, so
clients can detect drift.

## Consequences

- **Invariant:** the validator and every generated type read the *same*
  constraint definition. They cannot disagree by construction.
- Adding an output (a new SDK language, response validation) is "another
  consumer of the IR," not a parallel source of truth.
- Clients can assert compatibility against the contract hash.

## Alternatives considered

- **Hand-written OpenAPI / types**: drift from runtime behavior, the exact
  problem this avoids.
