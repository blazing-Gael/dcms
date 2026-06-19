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

## Near term (the rest of v0.x)

- Typed TypeScript SDK + typed query builder (codegen)
- Response validation; more client SDKs (Python, Dart)
- PostgreSQL adapter (production default) + pgvector
- Authentication (local + OIDC/SSO + bring-your-own) and RBAC at the gateway
- Relations (`expand`), i18n, draft/publish, soft delete
- Async embedding pipeline + semantic search
- Audit log, webhooks, rate limiting & query timeouts

## Toward v1.0

- Media pipeline, Wasm plugin runtime, admin UI, dashboard builder
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
