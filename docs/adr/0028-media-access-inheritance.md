# ADR-0028 — Media access by inheritance: `_media.access.read: inherit`

Status: Accepted
Date: 2026-09-16

## Context

`_media` takes a single `access:` block, so every file is readable by the same
principals regardless of what references it (GitHub #30). One DCMS instance
frequently needs both kinds of file at once — Agrojatra has shared editorial images
(readable by editors and the site-builder) *and* private user backups
(`backups.blob`, collection `read: owner`). With one media rule, **the builder token
and every editor can download every user's backup**: the collection's `owner` rule
protects the `backups` row, not the file its `blob` field points at.

The issue floated two shapes: named libraries (`media.libraries: {...}` + a field
picks one), or **inherit** — a file is readable iff the caller can read a record
that references it. Inherit keeps the rules on collections, where DCMS already puts
them, and needs no per-field wiring.

## Decision

Add a read-rule kind `inherit`, valid **only** as `_media.access.read`:

```yaml
_media:      { access: { read: inherit } }
articles:    { fields: { cover: { type: file } }, access: { read: public } }
backups:     { fields: { blob:  { type: file } }, access: { read: owner  } }
```

A media file is readable iff **the caller can read some record that references it**,
across every belongs-to and `many` (gallery) `file` edge in the schema; an
unreferenced upload is readable only by its uploader (`created_by`). The referencing
record's *own* read rule decides, per file — so editorial images (referenced by
public articles) are public, while a backup (referenced only by an owner-scoped row)
is readable only by that owner. No per-field access, no second library concept.

### Enforcement

- **Baseline** (`evalRule`): `inherit` resolves like `owner` — the uploader sees
  their own uploads, a media *list* is owner-scoped, and the library counts as
  gated (not world-public). This governs `/__media` listing and the cache posture.
- **Widening** (`mediaReadable`, on GET-one, `/raw`, and `?expand` of a `file`):
  beyond the uploader, the file is readable when `mediaReferencedReadable` finds a
  readable referencing record. That check queries each referencing collection with
  **its** read rule folded into the query — an `owner` collection contributes a
  `created_by = caller` filter, a `public` one contributes none, a `deny` one is
  skipped — so the reverse lookup never admits a record the caller couldn't read
  directly, and needs no post-filtering. `InverseRelations`/`InverseM2M` (already
  used by `?expand`) enumerate the edges.
- A denied media read is a **404**, never 403 (an owner boundary must not leak).

### Serving and caching

An inherited file is not statically public, so `/raw` always serves its own bytes
with `private, no-store` and never 302s to a public object URL — the same posture a
gated fixed rule already uses. That trades the CDN/redirect fast path (which would
hand out a rule-bypassing link) for correctness; a deployment that wants public,
cacheable images uses a fixed `read: public` rule instead.

### Validation

`inherit` anywhere but the top-level `_media` read rule — on another collection, on
a non-read action, or nested inside an `any:` — is a schema-compile error. It is
opt-in: unset, `_media` keeps its fixed rule (default public read), so every
existing schema is unchanged.

## Consequences

- Shared and private files coexist on one library with no per-file config; the leak
  in #30 is closed — a backup's bytes follow the owner rule on the row that
  references them.
- Cost: a gated inherited download does up to one small indexed query per
  referencing edge (usually one), and loses public byte-caching. Both are acceptable
  because inherit is opt-in; latency-sensitive public libraries keep the fixed rule.
- Reverse-lookup readability reuses the same `evalRule` decisions as forward reads,
  so the media boundary can't diverge from the record boundary.
- Out of scope: named libraries (subsumed by inherit for the driving case), and a
  per-file explicit ACL. The per-principal upload **quota** (issue #31) is a
  separate config-level control (`media.quotas`), not part of this access model.
