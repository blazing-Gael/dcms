# ADR-0011 — Media as a relation; bytes behind a pluggable blob store

**Status**: Accepted

## Context

An ecom-grade (or WordPress/Drupal-grade) CMS must let operators upload, reuse,
browse, replace, and serve files — product images above all. Two things have to
be true: assets must be *reusable* (one image referenced by many records, with a
"where is this used?" answer), and the *bytes* must live wherever the operator
wants — local disk for simple self-hosting, or any S3-compatible object store
(MinIO, SeaweedFS, Cloudflare R2, AWS S3, Backblaze B2, …) at scale.

Constraints from earlier decisions:
- The `store` interface is **locked** (ADR-0003) — bytes cannot flow through it.
- The schema is the **single source of truth** (ADR-0001) — a media field must
  derive its DB column, routes, validators, OpenAPI, and SDK types like anything
  else.
- Relations, expansion, referential integrity, and `on_delete` already exist
  (ADR-0010) and are worth reusing rather than duplicating.
- DCMS is **headless**: it ships an API + SDK, not a UI. A visual media library
  is a future dashboard concern that consumes this API.

## Decision

### Media is a relation, not a bespoke subsystem

Model file *metadata* as an engine-managed `_media` collection (a normal table:
`id, filename, content_type, size, storage_key, checksum, width, height, alt,
title, caption` + audit columns). A schema field of type `file` is **sugar**:
after a user schema validates, the engine injects the `_media` collection and
**rewrites every `file` field into a relation targeting `_media`** (`many: true`
→ a gallery via the m2m join table).

Consequences of the sugar: migration, `?expand=`, batched no-N+1 loading,
referential integrity, `on_delete`, nested reads, and SDK/OpenAPI typing all
apply to media **for free**. "Where is this asset used?" is just the reverse-
relation expansion built in ADR-0010's follow-up. `_media` is a reserved name
(users can't declare it) and is excluded from the public `/api/v1` routes — it is
served through dedicated media endpoints because its write path is bytes, not
JSON.

### Bytes live behind a pluggable `blob.Store`

A new, separate interface (not the locked `store`):

```go
type Store interface {
    Put(ctx, key string, r io.Reader, size int64, contentType string) error
    Get(ctx, key string) (io.ReadCloser, error)
    Delete(ctx, key string) error
    URL(key string) string // "" when the store is proxy-served (local)
}
```

- **Local-disk adapter** ships first (zero deps; simple self-hosting).
- **One S3-compatible adapter** ships next and reaches *every* S3-API store via
  config alone: `endpoint`, `region`, `bucket`, `force_path_style`,
  credentials (**env-only**, per the secrets rule), and a `public_base_url` /
  signed-URL TTL for serving. MinIO, SeaweedFS, R2, S3, B2, Spaces are all the
  same adapter with different config.

The response `url` field is **derived, not stored**: `blob.URL(key)` when the
backend serves directly (S3/R2), else the proxy route `/__media/{id}/raw`.

### Media endpoints (bytes path)

`POST /__media` (multipart upload → validate size/content-type → `Put` → write
row), `POST /__media/{id}` (**replace** the file, keeping the id and every
reference intact), `GET /__media` (the library: list with filters + pagination),
`GET /__media/{id}` (metadata), `GET /__media/{id}/raw` (stream with HTTP Range
for video/large files, or 302 to the object URL for S3), `PATCH /__media/{id}`
(edit alt/title/caption/filename), `DELETE /__media/{id}` (row **and** blob).

## Consequences

**Good**
- Reuses the entire relations/expand/integrity/codegen stack; the genuinely new
  code is small: the `_media` definition, the `blob.Store` + local adapter, and
  the media HTTP handlers.
- Assets are reusable and queryable; "where used" is free via reverse relations.
- Replace-in-place keeps references stable — the "swap out an image" workflow.
- Backend is a config switch across local and every S3-compatible store.
- Stays headless: a dashboard/admin UI later is a pure consumer of this API.

**Costs / deferred**
- Image derivatives (thumbnails / responsive sizes / on-demand transforms) are
  **not** in the first cut (phase M3). A media-library grid and responsive
  delivery will want them; the fork (pre-generate fixed sizes vs on-demand
  cached transforms) is deferred to that phase.
- Private/access-controlled files (signed URLs, auth on `/raw`) wait on auth
  (M4); the `storage_key`/serving split is designed to allow it without rework.
- Orphaned-blob GC, folders/tags, bulk ops, and content-hash dedup are later
  polish.
- `_media` needs light special-casing (custom write path, excluded from public
  routing and from generated writable inputs) despite being a normal collection.

## Related
- ADR-0001 (schema is source of truth), ADR-0003 (locked store — bytes go
  around it), ADR-0010 (relations/integrity that media reuses).
