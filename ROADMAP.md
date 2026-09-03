# DCMS Roadmap

This is the high-level direction. For the detailed, phased build plan with
acceptance criteria, see [`docs/DEV_ROADMAP.md`](./docs/DEV_ROADMAP.md).

> Status: **v0.1** — core engine taking shape. APIs and the schema language may
> change before v1.0. We plan to relicense from MIT to Apache-2.0 at v1.0.

## Where we are

The vertical slice works: write a `dcms.schema.yaml`, run `dcms dev`, and get a
validated, paginated REST API with an OpenAPI spec and interactive docs — no Go
required.

**Done**
- `store` layer + SQLite adapter (CRUD, filters, sort, keyset pagination,
  aggregation, transactions, introspection & migrations)
- Schema parser, validation, and compilation to tables
- Virtual REST router with the standard response envelope
- Server-side request validation
- OpenAPI 3.1 spec + contract hash (versioned contracts)
- Interactive API docs (`/__docs`)
- `dcms` CLI: `dev`, `validate`, `migrate`
- Local authentication + RBAC at the gateway: the `access:` authz spine,
  field-level access, composite (`any:`) rules, opaque DB-backed sessions
  (ADR-0016)
- Account lifecycle: self-registration, password change, password reset over
  email, logout-all, users admin API, user status (ADR-0019)
- Relations + `expand` (incl. nested), draft/publish, soft delete (ADR-0012)
- Revisions / version history (ADR-0013)
- Rich text: structured-JSON field with per-field allowlists (ADR-0014/0015)
- Media pipeline: file fields → `_media` relation, blob storage (local + S3)
  (ADR-0011/0015)
- Money / decimal type (ADR-0017)
- Idempotency keys for POST-create (ADR-0018)
- Request hardening: body-size cap, per-request timeout, rate limiting;
  configurable CORS + native TLS

## Near term (the rest of v0.x)

- Typed TypeScript SDK + typed query builder (codegen)
- Response validation; more client SDKs (Python, Dart)
- PostgreSQL adapter (production default) + pgvector
- External identity: a public bring-your-own `Authenticator` seam +
  OIDC/JWKS (ADR-0016 M4; ADR-0020)
- Audit log, webhooks / change feed, query timeouts
- Async embedding pipeline + semantic search
- i18n

## Toward v1.0

- Wasm plugin runtime, admin UI, dashboard builder
- Unix-socket transport, deploy tooling
- Relicense to Apache-2.0

## Later / Enterprise

- Couchbase adapter, multi-node cluster mode
- CRDT collaborative editing, MCP server, SAML/SSO
- Embedded library mode (CGO / N-API)

## Non-goals (for now)

- DCMS itself is **not** multi-tenant — the hosted offering is backend-per-customer.
- We are a headless backend, not a storefront platform; commerce is a vertical
  built *on* DCMS.

Roadmap items are directional, not commitments, and will shift with feedback.
