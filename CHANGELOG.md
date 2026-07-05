# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While on **0.x**, minor versions may include breaking changes.

## [Unreleased]

### Added
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
