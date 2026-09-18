# ADR-0031 — Extension hooks: synchronous, in-transaction business logic on generated endpoints

Status: Accepted
Date: 2026-09-18

## Context

DCMS has two extension seams and a gap between them. `Authenticator` (ADR-0020)
swaps *identity*. Events + webhooks (ADR-0021) react *asynchronously, at-least-once,
after commit*. Neither can do the thing every real application eventually needs:
**synchronous, in-request business logic** — reject a write on a business rule,
derive a value before it is stored, or perform a side-effect atomically with the
triggering write. Events fire after the fact and cannot veto; the `access:` rules
decide *who* may write, not *whether a particular value is allowed*.

Today the only route is to fork the engine or reimplement a generated endpoint,
which throws away the generated contract. The reserved `hooks:` directive
(ADR-0025) was the placeholder for closing this gap.

A tempting over-build looms here: a full plugin runtime (out-of-process RPC, a WASM
sandbox, a capability wire format, a marketplace). That machinery earns its place
only when the code being run is **untrusted and distributed** — a third-party plugin
an operator installs without auditing. DCMS is tier-2 single-tenant, backend-per-
customer (ADR-0022 context): the operator controls the binary, so the near-term need
is *their own* logic, compiled in. This ADR deliberately scopes to that and no more,
while making one cheap choice that keeps the larger door open.

## Decision

Add **in-process Go hooks** that run on the write lifecycle of every endpoint the
schema generates, plus a sibling mechanism for **custom routes**. Both are opt-in,
registered on `gateway.Options`, and off by default. No new store surface; the store
stays locked (ADR-0003).

### 1. Hooks run on the generated endpoints

A hook is a Go function registered per `(collection, event)`. The events are the
write lifecycle only:

    BeforeCreate  AfterCreate
    BeforeUpdate  AfterUpdate
    BeforeDelete  AfterDelete

Because these fire in the gateway's existing write path, they apply to the
**generated** `POST`/`PATCH`/`DELETE /{collection}` routes automatically — the
generated API becomes programmable without replacing it. A `Before*` hook may
mutate the incoming record or reject the write; an `After*` hook reacts to a
completed one. Registration order is the execution order.

`AfterRead`/response-shaping is **out of scope** (see Non-goals): a hook that added
fields to a response would break the "schema is the single source of truth"
invariant (ADR-0001). Read-time derived fields belong to the declarative `computed`
type, not to imperative hooks.

### 2. Hooks participate in the write transaction

`Before*` hooks run **inside the same transaction** the gateway already opens for
revisions, events, and idempotency. A hook error rolls the whole thing back and
maps to a structured 4xx — no partial write. A hook's own writes (e.g. decrement
inventory on order create) commit atomically with the trigger. This in-transaction
participation is precisely what events cannot offer, and it is the reason hooks are
in-process rather than a callout.

### 3. Hooks are identity-safe

A hook receives the **verified** `auth.Principal` (ADR-0020) read-only. It never
sets `created_by`/`updated_by`; the adapter still auto-stamps from the verified
identity (ADR-0016 audit invariant). A hook runs with server authority for its own
store operations — it is the operator's trusted code — but it cannot forge *who*
triggered the request.

### 4. Hooks receive a narrow interface, not the raw adapter

A hook is handed a small, purpose-built context — the verified principal, the
collection name, the parsed schema, and a **narrow store interface** exposing only
the operations a hook needs (a scoped read, and writes that enroll in the current
transaction) — not the full `store.Adapter`.

This narrowing is justified on its own merits today: least privilege, testability,
and a stable surface a hook is written against. It is *also*, and not by accident,
the seam any future transport would swap. That is the single forward-compatibility
choice this ADR makes, and it costs nothing now because a narrow interface is the
right design regardless.

### 5. Custom routes (sibling mechanism)

For logic that is a *new* endpoint rather than an augmentation of a generated one
(`POST /checkout`, a composite read), a host registers a custom route
(`method`, `path`, handler). The handler mounts **inside** DCMS's middleware stack,
so it inherits identity resolution, body caps, timeouts, and rate limiting, and
receives the same narrow context hooks get. A custom route that should appear in the
generated contract carries its own OpenAPI fragment; otherwise it is undocumented by
design (an internal endpoint).

## Non-goals (explicitly deferred)

- **Out-of-process (RPC) and WASM transports.** These add polyglot authorship and,
  for WASM, a sandbox for *untrusted* code — value that materializes only with a
  plugin marketplace or a hard requirement to write logic in another language. When
  that is a committed goal, the narrow interface of §4 becomes the contract those
  transports serialize; a raw `store.Adapter` would have foreclosed it. Not built
  here.
- **A capability wire format / WIT interface / marketplace / license enforcement.**
  Same reason. Designing a serialization boundary for transports we are not building
  is speculative generality; we build the in-process seam and stop.
- **`AfterRead` / response shaping.** Belongs to the declarative `computed` field
  type (contract honesty), not imperative hooks.

## Consequences

- The generated, schema-derived API gains real business logic while keeping its
  generated OpenAPI/SDK contract — hooks that change **values** are contract-safe;
  hooks may not change response **shape** (that is the schema's job). Values-not-
  shape is the guardrail that keeps ADR-0001 intact.
- Hooks and events become complementary, not competing: **hooks** are synchronous,
  transactional, in-request rules and derivations; **events** are asynchronous,
  retried, cross-system reactions (ADR-0021). A feature picks the one whose
  semantics it needs.
- **Issue #27 (value-constrained write rules) is the declarative front-end of this
  mechanism.** A common pattern — "an author may set `status: draft`, only an editor
  `published`" — should be expressible in the schema and compiled by core, with
  hooks as the imperative escape hatch for what a DSL will not express. Build the
  mechanism; absorb the frequent patterns declaratively afterward.
- The `hooks:` schema directive graduates from reserved to declaring *which* events a
  collection participates in (introspectable in `/__schema`, notable in codegen),
  while the *implementation* registers in Go. Declarative shape, imperative logic.
- This is trusted, compiled-in code by construction. A future untrusted-plugin story
  is a transport under the §4 interface, decided separately, and does not change any
  contract set here.
