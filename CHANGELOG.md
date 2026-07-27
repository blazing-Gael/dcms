# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While on **0.x**, minor versions may include breaking changes.

## [Unreleased]

### Added
- Authentication & authorization — milestone 1 (ADR-0016): the first
  security-bearing layer of the engine.
  - **Authorization** — collection `access:` rules are now live and enforced at
    the gateway: per-operation `read`/`create`/`update`/`delete`, each one of
    `public` | `authenticated` | `[role, …]` | `owner`. Denied writes → `403`
    (`401` if anonymous); denied single reads → `404` (existence never leaks);
    `owner` on a list narrows the query to the caller's own rows. Default when
    omitted: reads `public`, writes `authenticated`. Roles named in a rule must be
    declared under `auth.roles` (else a schema-compile error).
  - **Authentication** — a `principal` (id + roles) is resolved once per request
    by a pluggable `Authenticator` seam and feeds `store.WithActor`, so
    `created_by`/`updated_by` come from the *verified* identity. Local identity
    uses **opaque, DB-backed sessions** (not JWT — see ADR-0016 for the rationale
    in a backend-per-customer model): `POST /auth/login` issues a token (body +
    `HttpOnly` cookie), `POST /auth/logout` revokes it immediately, `GET /auth/me`
    returns the principal. Passwords are bcrypt-hashed; `password_hash` is stripped
    at every serialization choke point.
  - **Engine collections** `_users` and `_sessions` are injected (reserved, not
    JSON-CRUD routable). `auth:` config gains `provider`, `roles`, and
    `session.ttl`; the reserved `jwt:`/`oidc:` blocks are for the later
    external-identity milestone.
  - **Bootstrap** — `dcms admin create` writes the first admin directly; on first
    run an env-seeded admin (`DCMS_ADMIN_EMAIL`/`DCMS_ADMIN_PASSWORD`, env-only
    secrets) is created when no users exist yet.
  - **Contracts** — OpenAPI gains the `/auth/*` paths, a bearer/cookie security
    scheme, and per-operation `x-access` + `security`; the TS client gains an
    `auth` namespace (`login`/`logout`/`me`) with `Principal`/`LoginResult` types.
- Authentication & authorization — milestone 2 (ADR-0016): **field-level access**
  via `fields.<name>.access` with the same rule grammar as collection access.
  - **`read` masks** — an unauthorized reader still gets the record, just without
    the field (single, list, and `?expand`ed responses alike). Masking runs at the
    serialization choke points *after* response-contract validation.
  - **`write` filters** — an unauthorized writer's value for the field is silently
    dropped, not rejected, so round-tripping a masked record never `4xx`s. On
    create an `owner` write rule collapses to `authenticated`; on update it is
    checked against the stored `created_by`, loaded lazily (no extra read unless
    such a field is actually present in the body).
  - Enforced only when auth is enabled and the collection declares a field rule
    (`HasFieldAccess`), so the common path pays nothing. Only `read`/`write` keys
    are valid; unknown keys and undeclared roles are schema-compile errors.
  - **Contracts** — a field's JSON Schema gains `x-access-read`/`x-access-write`
    annotations so generated clients/docs know a field may be absent or ignored.
  - **Reference schema** — `examples/farmly.schema.yaml` now demonstrates a read
    mask (`orders.payment_reference` → admin-only) and write masks
    (`reviews.status`, `reviews.is_verified_purchase` → admin-only, so a customer
    cannot self-approve or self-verify their own review).
  - **Reference schema (M1)** — `examples/farmly.schema.yaml` also declares `auth:`
    (roles `admin`/`vendor`/`customer`, 7-day sessions) and a real `access:`
    policy per collection: public storefront reads, role-gated authoring,
    admin-only PII, and `owner`-scoped orders. A new schema test compiles every
    shipped example and pins farmly's access wiring.
- Record lifecycle (ADR-0012): opt-in per collection via `publishing: true`
  and/or `soft_delete: true`.
  - **Publishing** adds engine-managed `_status` (draft/published/archived) and
    `_published_at`. Public reads show only live content (`_status=published AND
    _published_at<=now`); a future `_published_at` **schedules** a go-live with no
    background job (visibility is a read-time comparison). Transition endpoints:
    `POST /{collection}/{id}/publish` (optional `{"at": "<RFC3339>"}` to schedule),
    `/unpublish`, `/archive`.
  - **Soft-delete** adds `_deleted_at`: `DELETE` trashes the row (hidden from
    reads), `POST /{id}/restore` brings it back, and `DELETE ?purge=true` removes
    it for good (still honoring `on_delete: restrict`).
  - The default visibility filter applies everywhere — lists, get-one (hidden →
    404, never 403), and relation expansion (batched, no N+1; a belongs-to to a
    hidden record keeps the id but isn't inlined).
  - An admin/preview bypass is gated by a secret `DCMS_PREVIEW_TOKEN` (env-only):
    a request with `X-DCMS-Preview: <token>` may pass `?status=` /
    `?include_deleted=` to see drafts/scheduled/archived/trashed content; without
    it these params are ignored.
  - Clients cannot set the managed columns directly (stripped from create/update).
  - The generated SDK gains `publish/unpublish/archive`, `restore`, a `purge`
    option, and preview `find` options; managed columns appear as readonly record
    fields; OpenAPI documents the transition routes.
- Version history / revisions (ADR-0013): opt-in per collection via
  `revisions: true`.
  - Adds an engine-managed `_revisions` collection (injected only when at least
    one collection opts in) that stores a **full JSON snapshot** of the record on
    every write, labeled with the operation (`create` / `update` / `publish` /
    `unpublish` / `archive` / `restore` / `delete`) and a per-record incrementing
    `version`.
  - Snapshots are captured **synchronously in the write's own transaction**, so
    history can never diverge from the record — either both commit or both roll
    back.
  - Endpoints (preview-gated by `DCMS_PREVIEW_TOKEN`, 404 when the collection
    lacks the directive): `GET /{collection}/{id}/revisions` (history, newest
    first, without the heavy blobs), `GET /{collection}/{id}/revisions/{version}`
    (one version incl. its snapshot), and `POST
    /{collection}/{id}/revisions/{version}/restore`.
  - **Restore is content-only**: it rolls back declared fields but leaves managed
    lifecycle columns (`_status` / `_published_at` / `_deleted_at`) untouched, and
    is itself recorded as a new revision (append-only history).
- Rich content field type `richtext` (ADR-0014): structured, portable-text-style
  body content.
  - The value is a **structured JSON document** (an array of blocks/spans plus
    custom `image` / `reference` / `code` / `embed` blocks) — not HTML or markdown
    — so it renders to any target and its embeds are first-class references.
    Stored in a JSON column; the locked store interface is untouched.
  - Configurable per field via `styles` / `marks` / `blocks` allowlists (with
    sensible defaults); unknown marks/blocks fail schema validation.
  - Validated on write: well-formed document within the field's allowlists, span
    marks resolve to a decorator or a declared `markDef`, and link hrefs must use a
    safe scheme (no `javascript:`). Every in-content reference (image → `_media`,
    `reference` nodes → the named collection) is checked for existence in the
    **same batched pass as relations** (field-named `422` on a dangling ref; no
    N+1). The generated SDK gains a shared `RichText` type; OpenAPI a `RichText`
    component.
  - `?expand=<richtext field>` resolves referenced records (image → its `_media`
    asset with a `url`; `reference` nodes → the named record) into a
    **deduplicated `included` manifest at the response root** (ADR-0015), keyed
    `"<collection>:<id>"`; the document AST stays id-only. Keeps documents
    cacheable, dedupes shared references across a list, and makes reference cycles
    harmless. Batched (one query per target collection, no N+1) and lifecycle-aware
    (a hidden/missing target is simply absent from `included`). The SDK gains an
    `Included` type and a `resolveRef` helper.
- Reference resolution via a manifest (ADR-0015): resolved references now ride a
  root `included` map instead of being inlined into the response, so updating one
  referenced entity no longer staleness-poisons every document that embeds it.
  Media is returned as a **public projection** everywhere it is serialized — the
  internal `storage_key` (blob path) is never exposed.
- Read hardening: list total is now opt-out via `?count=false` (skips the
  `COUNT(*)` scan; `meta.total` is then omitted); an **expand budget** caps the
  number of expand fields, nesting depth, and total resolved references (`422` when
  exceeded); and GET reads carry a strong **`ETag`** with `If-None-Match` → `304`
  conditional-GET support.
- `json` (and `richtext`) columns now round-trip structured values correctly: the
  SQLite adapter persists objects/arrays as JSON text on write, and the gateway
  decodes them back to real structure on read (previously a structured `json`
  value could not be written).
- Store: `IsNull` / `NotNull` filter operators (additive) for null-testing any
  nullable column.
- Media library (ADR-0011): a `file` field type (`file` / `file, many: true`)
  that is sugar for a relation to an engine-managed `_media` collection, so file
  assets reuse the whole relation stack — reference, expand, referential
  integrity, `on_delete`, and typed SDK output — and answer "where is this used?"
  via reverse expansion. Bytes live behind a pluggable `blob.Store` (local-disk
  adapter now; one S3-compatible adapter — MinIO, SeaweedFS, Cloudflare R2, AWS
  S3 — lands next behind the same interface). Endpoints under `/__media`: upload
  (multipart), replace-in-place (keeps the id and every reference), list/filter
  (the library), metadata edit, delete (row + bytes), and range-aware raw
  streaming (or a redirect for direct-serving backends). A derived `url` is added
  to every media record — version-stamped from the checksum so a replace-in-place
  busts CDN/browser caches — and the raw endpoint sets an `ETag` (+ `Cache-Control`
  and `304` revalidation). Configured via a `media:` block; the generated SDK
  gains a typed `media` client (`upload`/`replace`/`list`/`get`/`delete`/`rawUrl`).
- S3-compatible media backend (`media.driver: s3`): a single adapter for MinIO,
  SeaweedFS, Cloudflare R2, AWS S3, Backblaze B2, and the rest — selected by
  config (`endpoint`, `region`, `bucket`, `force_path_style`, `public_base_url`)
  with credentials supplied via env vars only (`DCMS_S3_ACCESS_KEY` /
  `DCMS_S3_SECRET_KEY`). With `public_base_url` set, media URLs point straight at
  the bucket/CDN (bytes bypass the server); otherwise they proxy through
  `/__media/{id}/raw`. Pure-Go client — the single static binary is unchanged.
- Relations: `belongs-to` (`type: relation, target: …` → string FK column) and
  `many-to-many` (`many: true` → engine-managed join table with a unique link
  index and full audit columns). Referenced by id; required relations enforced.
- Relation expansion via `?expand=`: belongs-to inlines the target object
  (batched on lists, no N+1), auto-derived has-many inverses and m2m links expand
  on single-record reads. m2m link sets are written/replaced transactionally.
  Reverse many-to-many expansion (e.g. `tags?expand=posts`) and nested/multi-hop
  expansion (e.g. `?expand=author.company`) are supported on single-record reads;
  nested expansion is rejected on lists to avoid multi-level N+1.
- Nested/inline writes: a relation may be given as an inline object (or, for
  many-to-many, a list mixing ids and objects) and the related record(s) are
  created and linked in the same request, transactionally — a nested write is
  all-or-nothing. SDK inputs type this as `string | CreateTarget`; OpenAPI
  documents it as an `anyOf`.
- Two-layer referential integrity (ADR-0010): a gateway **validation layer**
  batch-checks every belongs-to and many-to-many reference before a write and
  returns a field-named `422` for anything that doesn't resolve (one `IN` query
  per collection — no N+1); a store **protection layer** emits real database
  foreign keys at `CREATE TABLE` (belongs-to → `REFERENCES`, join tables →
  `ON DELETE CASCADE` for free link cleanup). Deleting a still-referenced record
  is the `RESTRICT` default → `409`. Validation never relies on DB errors for
  user-facing responses; a FK violation on a write is treated as an invariant
  breach (logged `500`). Belongs-to relations take a configurable
  `on_delete: restrict` (default) `| cascade | set null` policy, enforced by the
  database foreign key. The migrator introspects existing foreign keys and
  refuses with a clear, actionable error when a relation change would need a
  table rebuild (adding/changing an FK on an existing column, or retrofitting a
  required column) rather than failing cryptically.
- Multi-language codegen scaffolding: a language-neutral model + `Backend`
  interface, so new SDKs are a type map + template (TypeScript is the first).
- `store` storage abstraction with a pure-Go SQLite adapter: CRUD, eq/comparison/
  `in`/`contains` filters, sort, sparse fieldsets, keyset cursor pagination,
  count/sum/avg aggregation, transactions, and introspection-driven migrations.
- Configurable, injectable id generation (UUIDv7 default).
- Engine-managed audit columns: `created_at`, `updated_at`, `created_by`,
  `updated_by`, with actor attribution via request context.
- Schema parser, validator, and compiler (`dcms.schema.yaml` → tables).
- Virtual REST router: per-collection CRUD, list query params, the standard
  response envelope, and error mapping.
- Server-side request validation (required/type/min/max/pattern/enum).
- OpenAPI 3.1 spec at `/__openapi` and a contract hash (`ETag` / `info.version`).
- Interactive API documentation at `/__docs`.
- Introspection/probe endpoints: `/__schema`, `/__health`, `/__ready`.
- `dcms` CLI: `dev`, `validate`, `migrate`, `codegen`, `version`.
- Layered configuration (ADR-0009): a `dcms.config.yaml` plus `DCMS_*` env vars,
  with precedence flags > env > file > defaults; `--config` flag on every command.
- TypeScript codegen (`dcms codegen --lang ts`): a fully-typed client module —
  per-collection interfaces, `Create`/`Update` input types, typed filters/sort,
  and a `fetch`-based runtime client — stamped with the contract version.
  Relations are typed by context: inputs take ids (`author: string`,
  `tags?: string[]`) while responses hold the id or the expanded object
  (`author: string | Users`, `tags?: Tags[]`). OpenAPI documents the `?expand=`
  parameter, the id-or-object response shape, and the delete `409`.
- Response shaping: outgoing records are normalized to the schema's declared
  JSON types (e.g. SQLite 0/1 → real `true`/`false` booleans).
- Strict response validation (`server.validate_responses`, ADR-0008): verifies
  every returned record against the schema and answers 500 rather than shipping
  non-conforming data. Defaults on under `dcms dev`, off otherwise.

### Fixed
- Boolean fields were returned as `0`/`1` integers instead of JSON booleans,
  contradicting the schema, OpenAPI spec, and generated SDK types.
- SQLite `PRAGMA foreign_keys` was set once on the pool, so it applied to only
  one connection and left referential integrity unenforced on the others; it is
  now set via the DSN so it holds on every pooled connection.

### Changed
- Engine packages moved under `/internal` (not importable from outside the
  module) so the binary, HTTP API, and SDKs are the only public surface; the Go
  package API stays unstable until a facade is deliberately promoted out.

[Unreleased]: https://github.com/blazing-Gael/dcms/commits/main
