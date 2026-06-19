# Architecture Decision Records

This directory holds **ADRs** — short, numbered, immutable records of the
significant decisions behind DCMS: the context, the decision, and its
consequences. They answer "why is it built this way?" for contributors and our
future selves.

## Conventions

- One decision per file, named `NNNN-kebab-title.md`.
- ADRs are **append-only**. Don't rewrite history — if a decision changes,
  add a new ADR and mark the old one `Superseded by ADR-XXXX`.
- **Status**: `Proposed` · `Accepted` · `Superseded` · `Deprecated`.
- Use [`0000-template.md`](./0000-template.md) as the starting point.

## Index

| # | Decision | Status |
|---|----------|--------|
| [0001](./0001-schema-is-single-source-of-truth.md) | Schema is the single source of truth | Accepted |
| [0002](./0002-configurable-except-essentials.md) | Everything configurable except audit/trail/timestamp essentials | Accepted |
| [0003](./0003-store-interface-auth-agnostic.md) | Locked, auth-agnostic `store` interface; authz at the gateway | Accepted |
| [0004](./0004-uuidv7-ids.md) | UUIDv7 ids by default, via an injectable generator | Accepted |
| [0005](./0005-audit-columns-and-actor.md) | Audit columns always present; actor attribution from context | Accepted |
| [0006](./0006-rest-first-api-surface.md) | REST + OpenAPI is the core; GraphQL & MCP are derived/optional | Accepted |
| [0007](./0007-backend-per-customer.md) | Hosted Tier-2 is backend-per-customer, not multi-tenant | Accepted |
| [0008](./0008-contracts-derive-from-schema.md) | Validators, OpenAPI, and SDKs all derive from the schema | Accepted |
