# ADR-0003: Locked, auth-agnostic `store` interface; authorization at the gateway

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

The engine must support multiple databases (SQLite, Postgres, Couchbase) and
keep authorization logic in one place rather than scattered across handlers.

## Decision

Define a single storage interface, `store.Adapter` (see
[`docs/STORE_INTERFACE.md`](../STORE_INTERFACE.md)), that every adapter
implements. It is **locked after Phase 1** — no breaking changes without a major
version. The `store` layer is **authorization-agnostic**: it trusts its caller.

**Trust invariant:** everything that reaches `store` must first pass an
authorization gate — the HTTP **gateway** for requests, and the **plugin
host-ABI gate** for Wasm plugins. Plugin identity is *assigned by the trusted
host, never claimed by the plugin*; sandboxed plugins call host functions that
stamp identity and check manifest-declared scopes.

## Consequences

- Swapping databases is one config line; business logic is written once against
  `store` and is automatically transactional inside `Tx`.
- Authorization lives in exactly one layer, simplifying audit and review.
- The invariant must be enforced for *every* path into the store, including
  future ones (plugins, embedded library mode) — a standing review item.

## Alternatives considered

- **Authz inside the store/handlers**: scatters policy, easy to get
  inconsistent, hard to audit.
- **ORM / query builder as the seam**: leaks dialect details and over-couples
  business logic to SQL.
