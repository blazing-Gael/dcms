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
| [0009](./0009-layered-configuration.md) | Layered config: flags > env > file > defaults; one artifact per instance | Accepted |
| [0010](./0010-two-layer-referential-integrity.md) | Two-layer referential integrity: app-level validation + DB-FK protection where supported | Accepted |
| [0011](./0011-media-and-blob-storage.md) | Media as a relation to an engine-managed `_media` collection; bytes behind a pluggable blob store | Accepted |
| [0012](./0012-record-lifecycle.md) | Record lifecycle: publishing states (`_status` + `_published_at`) and soft-delete (`_deleted_at`), gateway-filtered with a preview-token bypass | Accepted |
| [0013](./0013-revisions.md) | Revisions: append-only version history in an engine-managed `_revisions` collection, full snapshots captured in the write transaction | Accepted |
| [0014](./0014-rich-content.md) | Rich content: a structured (portable-text-style) `richtext` field stored as JSON, with per-field allowlists and batched in-content reference validation | Accepted |
| [0015](./0015-reference-manifest.md) | Reference resolution via a deduplicated root `included` manifest (pure AST); media returned as a public projection | Accepted |
| [0016](./0016-authentication-and-authorization.md) | Auth: a principal spine + gateway-enforced `access:` rules (authz), with opaque DB-backed sessions for local identity (authn); `_users`/`_sessions` injected; JWT reserved for external OIDC | Accepted |
| [0017](./0017-money-decimal-type.md) | Exact `decimal` money type: string wire format, int64 minor-units storage, gateway-side conversion; currency/unit is a companion field | Accepted |
| [0018](./0018-idempotency-keys.md) | Idempotency keys for `POST` creates via `Idempotency-Key` header; durable `_idempotency` collection, reserve→execute→finalize in one Tx; opt-in cost | Accepted |
| [0019](./0019-account-lifecycle.md) | Account lifecycle: self-registration, password change/reset, users admin API, runtime roles, log-out-everywhere; thin identity vs. app profile; `Notifier` email seam | Accepted |
| [0020](./0020-external-identity-seam.md) | Public external-identity seam: `Principal`/`Authenticator` promoted to `pkg/auth` with a `Claims` bag and a shared `NewContext`/`FromContext` key, so bring-your-own auth works out-of-tree with no provider code and no new deps | Accepted |
| [0021](./0021-events-and-webhooks.md) | Events, change feed, and signed webhooks (M-B): an append-only `_events` outbox captured in the write transaction, exposed as an `id`-keyset change feed and (later phases) signed webhooks; opt-in per collection, store stays locked | Accepted |
