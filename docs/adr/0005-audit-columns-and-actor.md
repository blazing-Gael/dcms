# ADR-0005: Audit columns always present; actor attribution from context

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

Audit trails and timestamps are the non-optional essentials
([ADR-0002](./0002-configurable-except-essentials.md)). The store is
auth-agnostic ([ADR-0003](./0003-store-interface-auth-agnostic.md)), so it can't
know "who" from auth directly — yet attribution must work everywhere.

## Decision

Every collection always carries the audit columns `created_at`, `updated_at`,
`created_by`, `updated_by` (the schema compiler emits them regardless of the
`timestamps` directive). The adapter stamps them on write **when the column
exists**:

- timestamps are set/re-stamped automatically;
- `created_by`/`updated_by` come from a **trusted actor** carried in context
  (`store.WithActor` / `store.ActorFromContext`) — attribution only, never
  authorization. Client-supplied values for these are ignored.

## Consequences

- Audit trails are structural, not opt-in.
- A trusted caller (gateway from a verified principal, or the plugin host from a
  plugin's assigned identity) sets the actor; untrusted client input never can.
- The actor must be propagated into background jobs (e.g. embedding) so async
  work stays attributable.

## Alternatives considered

- **Gateway writes `created_by` into the record**: makes the field client-
  spoofable unless carefully stripped, and scatters the logic.
- **`timestamps:` toggles audit columns**: lets users disable accountability —
  rejected per ADR-0002.
