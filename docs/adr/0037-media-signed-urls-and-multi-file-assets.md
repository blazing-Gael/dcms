# ADR-0037 — Media signed URLs and multi-file assets

Status: Accepted
Date: 2026-10-22

## Context

A `_media` asset is one object at blob key `{id}`, and a private file is reachable
only through `/__media/{id}/raw` with the caller's credentials and a per-request
access check (ADR-0011, ADR-0028 `inherit`). Three gaps block real media delivery —
the edtech video phase first, but images/invoices/backups generally (GitHub #65):

1. **No derived files under an asset.** An HLS ladder (a playlist plus hundreds of
   segments) or image renditions have nowhere to live — one asset is one object.
2. **No way to deliver private media where credentials can't be sent:** `<img src>`,
   `<video src>`, a link in an email, a cross-origin frontend using bearer tokens, a
   CDN edge.
3. **No access check cheap enough for per-segment video.** A player fetches a segment
   every 2–6s per viewer; `inherit` (a referencing-record lookup per request) does not
   scale, and a CDN cannot cache auth-checked bytes.

The `blob.Store` interface is *not* locked (unlike `store` — ADR-0003), so it may grow.
The `local` backend's `safeKey` currently forbids `/`, so a multi-segment key layout is
a real change, not a config tweak.

## Decision

Three additions, plus a seam. Shipped together as the video MVP; the CDN/presign
implementations behind the seam are deferred.

### 1. Multi-file assets

- Blob key layout: the primary original stays at `{id}` (no migration of existing
  blobs); derived files live at `{id}/{path}`, with `{path}` a traversal-safe,
  multi-segment relative path (no `.` / `..` / leading slash / backslash).
- `blob.Store` gains `List(ctx, prefix) ([]string, error)` and
  `DeletePrefix(ctx, prefix) error`. Implemented for local (walk the `{id}/` subtree)
  and S3 (list/remove by prefix).
- Routes: `PUT /__media/{id}/files/{path}` adds a derived file (size/type caps, counted
  against the **asset owner's** quota, ADR-0028); `GET /__media/{id}/files` lists them;
  `DELETE /__media/{id}` removes `{id}` and the whole `{id}/` prefix. `Range`/`ETag`
  carry over from `/raw` unchanged to `/files/{path}` and the signed path.

### 2. Signed URLs

The access check runs once; the URL then carries its own proof, so later fetches need
no session and no DB:

```
/__media/{id}/s/{token}/{path}
token = exp . scope . base64(HMAC(secret, id | scope | exp | principal?))
```

- **Verify = HMAC + expiry only.** No DB, no session — cheap enough per segment, and a
  CDN can cache the bytes because the cache key excludes the token.
- **`scope` is a path prefix** (e.g. `hls/`), so one token covers a whole ladder.
- **Shareable by default** (not bound to a caller), so the CDN cache key is the same for
  every authorized viewer. A per-sign `bind_principal` option adds caller-binding for
  leak tracing / watermarking when a caller wants it. (Decision: shareable default.)
- **Auto-signed in responses.** `addMediaURL` — the single serialization choke point —
  emits a signed `url` for each private file field when the library is gated
  (`mediaReadGated`). An authorized record read thus already carries usable
  `<img src>` / playlist URLs. Public media keeps its plain URL.
- **Explicit signing:** `POST /__media/{id}/sign` (scope, ttl), after the normal access
  check, for players and hooks.
- **Expiry bucketing:** `exp` rounds up to a fixed window, so repeated reads return the
  *same* URL and browser/CDN caching keeps working. Window + TTL configurable.
- **Key rotation:** verify accepts current + previous secret; rotating out a secret
  revokes every URL signed with it. Secret is env-only (like `PreviewToken`).

### 3. Public package `pkg/sign`

Standard-library only, no dependency on the engine (like `pkg/auth`), so the planned
standalone streaming module — and any host — can depend on just this. It owns the token
format (a documented **wire contract** with conformance tests), `Sign`/`Verify`, key
rotation, and an http verify middleware. The token format is frozen and versioned.

### 4. `URLSigner` seam

```go
type URLSigner interface {
    // SignedURL returns a URL for {id}/{scope}* valid for ttl, optionally caller-bound.
    SignedURL(id, scope string, ttl time.Duration, principal string) string
    // Verify reports whether a presented token authorizes {id}/{path}.
    Verify(id, path, token string) bool
}
```

`Options.URLSigner` defaults to the `pkg/sign` HMAC implementation (DCMS verifies). CDN
token-auth (Bunny/Cloudflare — the edge validates, origin trusts a secret header) and
S3/R2 presigned GETs are additional implementations behind the same interface,
**deferred** to a later issue — only the seam and the HMAC default ship now.

## Consequences

- Private images/PDFs work directly in cross-origin `<img>`/links; video plays from a
  CDN with a per-viewer-shared, cache-friendly URL and no per-segment DB cost.
- Signed access composes with ADR-0036 membership: the record read that *mints* the URL
  is gated by `inherit`→membership; the URL then serves bytes without re-checking. The
  two features meet exactly at `addMediaURL`.
- `blob.Store` grows two methods (it is not locked); the token format is a new public
  contract that must stay backward-compatible once published.
- **Bounded on purpose.** HMAC verification trusts the secret absolutely — a leaked
  secret forges any URL until rotated; rotation is the mitigation. Scope is a path
  prefix, not a regex. CDN/presign remain future work behind the seam.
- A signed URL is a bearer capability for its TTL: anyone with the link can fetch within
  the window. That is the point (shareable), and the reason TTLs are short and
  principal-binding is available when leak tracing matters.

## Alternatives considered

- **Per-request auth on every segment (status quo / `inherit`).** A DB lookup per
  2–6s-per-viewer segment, uncacheable by a CDN. Does not scale; the gap this ADR closes.
- **One object per asset + a separate renditions collection.** More store surface and a
  second lifecycle to keep in sync with the asset; the `{id}/{path}` prefix keeps derived
  files owned by, and deleted with, their asset.
- **Signing inline in the gateway (no `pkg/sign`).** Less surface now, but the token
  format could not be depended on out-of-tree (the streaming module needs it) and the
  wire contract would not be independently testable.
- **Principal-bound tokens by default.** Leak-traceable, but a distinct URL per viewer
  defeats CDN cache sharing. Made opt-in instead.
