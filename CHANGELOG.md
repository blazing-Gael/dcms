# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While on **0.x**, minor versions may include breaking changes.

## [Unreleased]

### Added
- **Ownership by relation — `owner_field` access rule (issue #7).** An `access:`
  rule can now scope to a named relation instead of `created_by`, so a row that
  **belongs to** a user who did not create it is still reachable:
  `read: { owner_field: user }` narrows reads/writes to rows whose `user`
  relation equals the caller. It composes inside `any: [...]` (e.g.
  `{ any: [admin, { owner_field: user }] }`) and works on field-level rules too.
  A relation may now target the built-in `_users` table for real referential
  integrity (its expansion is redacted like everywhere else, so no password hash
  leaks). This unblocks the ordinary multi-user shapes — orders, subscriptions,
  assignments — where "belongs to" isn't "created by", including rows a service
  account writes for a user.
- **Webhook events now carry `from_status`.** A lifecycle transition event
  records both ends of the change (`from_status` and `to_status`), read in the
  write's own transaction; only transitions on event-emitting collections pay the
  extra read.
- **Durable account email — events, phase 3 of M-B (ADR-0021).** Password-reset
  email (and future account notifications) now goes through a durable outbox
  instead of being sent inline. `POST /auth/forgot` enqueues the message and
  returns immediately; a background worker delivers it via the configured
  transport (SMTP, or the dev log mailer) with **retries, backoff, and a
  dead-letter** state — so a slow or briefly-unavailable mailer no longer blocks
  the request or drops the email. A delivered notification's row is deleted on
  success, so the raw action link does not linger in the database.
- **Signed webhooks — events, phase 2 of M-B (ADR-0021).** Configure endpoints
  under `events.webhooks` and DCMS pushes each change event to them, built on the
  same `_events` log (which stays webhook-agnostic). Delivery is durable and
  runs entirely in the background: a worker fans new events into a per-endpoint
  queue and delivers them with **exponential backoff, retries, and a dead-letter**
  state after a configurable max. Payloads are **signed** — `X-DCMS-Signature`
  is an HMAC-SHA256 over `timestamp + "." + rawBody` (verify before parsing; the
  signed timestamp defeats replay), with `X-DCMS-Event` and a stable
  `X-DCMS-Delivery` id (the event id) for consumer dedup. Per-endpoint filters by
  event type and collection. HMAC secrets are **env-only** (`secret_env`). Admin
  ops: `GET /api/v1/_events/deliveries?status=dead` to inspect the dead-letter set
  and `POST /api/v1/_events/deliveries/{id}/retry` to re-arm one — so recovery is
  never "redeploy". At-least-once and unordered by design.
- **Change feed — events, phase 1 of M-B (ADR-0021).** A collection can opt into
  `events: true` to record every state change — create, update, delete, and the
  lifecycle transitions (publish/unpublish/archive/restore, soft-delete) — as an
  append-only row in a new engine-managed `_events` log, captured **in the same
  transaction as the write** so an event never disagrees with what happened.
  `GET /api/v1/_changes?since=<cursor>` serves them as an `id`-keyset feed, so a
  static-site generator or ISR frontend can poll for changes in O(changes) instead
  of scanning records or hot-looping on `updated_at`. Admin-only; opt-in per
  collection (a collection that doesn't declare `events:` writes no rows and costs
  nothing). Signed webhooks on top of this log are the next phase.

## [0.1.0-beta.2] - 2026-09-04

Install-channel fixes for the beta. The binaries and Docker image from beta.1 were
fine; this makes the one-line installers work during the pre-release phase and adds
Homebrew.

### Added
- **Homebrew (macOS):** `brew install blazing-Gael/tap/dcms`, published as a cask
  to the `blazing-Gael/homebrew-tap` tap on each release. The cask strips the
  macOS quarantine attribute so the unsigned binary runs without a Gatekeeper
  prompt.

### Fixed
- **Install scripts now work during the beta.** `install.sh` and `install.ps1`
  fetched from GitHub's `releases/latest/download/` path, which resolves only to
  the latest *stable* release and therefore 404'd while only pre-releases existed.
  They now resolve the newest release (pre-releases included) via the releases API
  and download from that tag.
- **Docker `:latest` now tracks the newest release during 0.x**, so
  `docker run ghcr.io/blazing-gael/dcms` resolves before a stable release exists
  (the tag was previously gated to non-pre-release versions only).

## [0.1.0-beta.1] - 2026-09-04

First public beta. DCMS now installs as a single prebuilt binary — no Go toolchain
— and goes from install to a running backend for your frontend in two commands.
SQLite-backed; suitable for blogs, small sites, and prototypes. The schema
language and API may still change before 1.0.

### Added
- **Prebuilt binary releases + one-line install.** Every release publishes
  checksummed archives for Linux, macOS, and Windows (amd64 + arm64) built with a
  pure-Go SQLite driver, so there is no cgo and no runtime dependency. Install with
  `curl -fsSL …/install.sh | sh` (macOS/Linux), `irm …/install.ps1 | iex`
  (Windows), a prebuilt binary from the releases page, or the
  `ghcr.io/blazing-gael/dcms` Docker image — none of which require Go.
- **`dcms init` — scaffold a runnable project in one command.** `dcms init [dir]`
  writes a starter `dcms.schema.yaml` (a `posts` collection), a `dcms.config.yaml`,
  a `.env.example` for the bootstrap admin credentials, and a `.gitignore`, then
  prints the next steps. The generated config **pre-allows the usual localhost
  frontend origins for CORS** (`:3000`, `:5173`) and serves on `:8080`, so a
  freshly-installed binary goes from `dcms init` to a working backend a separate
  frontend can call with just `dcms dev`. Refuses to overwrite an existing project
  unless `--force`. `dcms dev` now gives a friendly "run `dcms init`" hint when no
  schema is present instead of a raw file-not-found error.
- **Public external-identity seam — `pkg/auth` (ADR-0020).** The `Authenticator`
  interface and the `Principal` it returns are now a standalone public package
  (`github.com/blazing-Gael/dcms/pkg/auth`), so an external identity source
  (OIDC, a reverse proxy, a third-party auth service such as Appwrite) can be
  wired in without forking the tree. `Principal` gains a `Claims map[string]string`
  bag for identity-source-specific data (external subject, tenant, email) that the
  engine does not interpret. `auth.NewContext`/`auth.FromContext` expose the
  request principal through a single shared context key, for hosts wiring their
  own middleware around the gateway. No new dependencies, no new endpoints, and no
  change to authorization enforcement — the gateway consumes the public types via
  aliases, so existing callers are unaffected. (Closes the "bring your own auth"
  gap reported in issue #1.)

### Fixed
- **Rich text: the node cap is now enforced within a single block.** A document
  whose node count exceeded `maxRichTextNodes` was accepted when the overage sat
  inside one block (the cap was only checked between top-level blocks). It is now
  checked per span, so the documented bound holds. (Issue #3.)
- **Rich text: protocol-relative link hrefs are rejected.** `safeHref` treated
  `//host/path` as a relative path and allowed it, when a browser reads it as a
  cross-origin navigation inheriting the page scheme. Such hrefs are now rejected,
  so a validated href is genuinely same-origin or an allowlisted scheme. (Issue #4.)
- **Emails are now case-insensitive.** Addresses are validated and normalized
  (trimmed + lower-cased) on every account write and lookup, so a user who
  registered as `Ann@Example.com` can log in as `ann@example.com`, and case
  variants no longer create duplicate accounts. Validation also rejects malformed
  addresses (including CR/LF), closing an email-header-injection path into the
  reset mailer. (Found by end-to-end auth testing.)
- **The last active admin can no longer be removed.** Demoting, disabling, or
  deleting a user who is the only remaining active admin is refused with a `409`
  (`LAST_ADMIN`), so an operator can't accidentally lock the whole backend out of
  administration. With two or more admins, self-demotion is allowed as before.

### Added
- **Accounts — Phase 1: local account lifecycle (ADR-0019).** Self-service and
  admin operations over the identity spine, all reusing opaque DB sessions so
  revocation is immediate.
  - **Self-service** (`/auth`): `POST /register` (self-registration, **off by
    default**, config-gated with a non-admin default role; enumeration-safe),
    `POST /password` (change own password; revokes the caller's *other*
    sessions), `POST /logout-all` (revoke all own sessions).
  - **Admin** (`/admin/users`, gated by configurable `auth.admin_roles`, default
    `[admin]`): list / create / get / update (name, roles, status, password
    reset) / delete, plus `POST /{id}/logout-all`. Role changes are validated
    against the schema-declared roles; `password_hash` is never serialized.
  - **User `status`** (`active`/`disabled`) added to `_users`: a disabled account
    is refused login with the same flat `401`, and admins can suspend without
    deleting history. Additive column, no backfill (empty ⇒ active).
  - **Password policy:** configurable minimum length (`auth.password.min_length`,
    default 8) and a hard 72-byte cap (bcrypt truncates beyond it).
- **Accounts — Phase 2: password reset via email (ADR-0019).** `POST /auth/forgot`
  (always `200`, enumeration-safe — a link is minted and sent only for a real,
  active account) → `POST /auth/reset` (`{token, new}`). Reset tokens are
  single-use, short-TTL (default 1h), stored **hashed** in a new engine-managed
  `_auth_tokens` collection; a successful reset revokes **all** of the user's
  sessions. Delivery goes through a pluggable **`Notifier`** seam: a dev-log
  mailer by default (prints the link to the console — reset works with zero
  config) or SMTP when `auth.smtp.host` is set (STARTTLS; credentials env-only).
  Expired tokens are swept in the background alongside idempotency keys. Email
  verification remains a later increment (the `_auth_tokens` table already
  supports a `verify` purpose).
- **Edge/serving readiness: CORS, native TLS, and proxy-aware secure cookies.**
  - **CORS** — configurable cross-origin access (`server.cors`), off by default
    (empty `allowed_origins` ⇒ same-origin only). Preflight `OPTIONS` is answered
    `204` before auth or rate-limiting; credentialed CORS correctly echoes the
    specific origin with `Vary: Origin` (a wildcard `*` is used only without
    credentials). Sensible default methods/headers; `DCMS_CORS_ALLOWED_ORIGINS`
    env override.
  - **Native TLS** — set `server.tls.cert_file` + `key_file` (or the `DCMS_TLS_*`
    envs) to serve HTTPS directly; otherwise DCMS serves HTTP for a
    TLS-terminating proxy (the recommended topology). No cert management/renewal.
  - **Proxy-aware secure cookie (fix)** — the session cookie's `Secure` flag now
    also honors `X-Forwarded-Proto: https` when `server.trust_proxy` is set, so a
    proxy-terminated HTTPS deployment still marks the cookie HTTPS-only (previously
    it only saw direct TLS). `trust_proxy` moved from `server.rate_limit` up to
    `server` since it now governs both IP keying and cookie security.
- **Idempotency keys (M-A, ADR-0018).** A `POST` create carrying an
  `Idempotency-Key` header is safe to retry: the original response is replayed
  (with `Idempotent-Replay: true`) instead of creating a duplicate. Reusing a key
  with a different body is a `422`; a still-in-flight duplicate is a `409`. Keys
  are recorded durably in an engine-managed `_idempotency` collection and honored
  for a TTL (default 24h); reserve → create → finalize run in one transaction so a
  concurrent duplicate can never execute twice, and a failed create leaves the key
  free to retry. On by default (inert until a client sends the header;
  `server.idempotency` / `DCMS_IDEMPOTENCY_ENABLED` to configure); expired rows
  are swept in the background. The header is documented in the generated OpenAPI.
  **This completes the M-A hardening milestone.**
- **Rate limiting (M-A).** Two in-memory token-bucket tiers, on by default with
  generous limits (`server.rate_limit`; `enabled: false` or
  `DCMS_RATE_LIMIT_ENABLED=false` disables). The **API** tier is keyed per
  authenticated principal (client-IP fallback) so one client's flood never
  throttles another (default ~100 req/s, burst 200); the **auth** tier is keyed
  per client IP and guards the unauthenticated `/auth` endpoints (default 60/min,
  burst 20). Over-limit requests answer `429` (`RATE_LIMITED`) with a
  `Retry-After` header; health probes and media byte routes are never limited.
  `trust_proxy` (`DCMS_TRUST_PROXY`) opts into X-Forwarded-For keying for
  deployments behind a trusted proxy. The `RateLimiter` interface leaves room for
  a distributed backend later without touching call sites.
- **Request hardening (M-A): body-size cap + per-request timeout.** JSON request
  bodies on the collection API and `/auth` are now capped (default **1 MiB**,
  `server.max_body_bytes` / `DCMS_MAX_BODY_BYTES`); an over-cap body is a `413`
  (`PAYLOAD_TOO_LARGE`) instead of an unbounded buffer. Every such request also
  carries a deadline (default **15s**, `server.request_timeout_seconds` /
  `DCMS_REQUEST_TIMEOUT_SECONDS`; negative disables); a query that outlives it is
  abandoned and returns `504` (`TIMEOUT`) rather than pinning a connection. Media
  byte uploads keep their own `max_upload_bytes` and are exempt from both. The
  HTTP server also gained a 10s `ReadHeaderTimeout` (Slowloris guard).
- **`decimal` — exact fixed-point money type (ADR-0017).** A new scalar field
  type for values floats can't represent safely (prices, taxes, totals). Declared
  with an optional `scale` (fractional digits, 0–9, default 2). On the wire it is
  a **quoted string** (`"12.50"`), never a JSON number — a bare number is rejected
  (`422`) because it is already a lossy float on arrival, and excess precision is
  rejected rather than silently rounded. Stored as an exact int64 count of minor
  units, so sort, range filters (`filter[price][gte]=10.00`), and `SUM` are exact;
  the store stays type-agnostic (all conversion is gateway-side, mirroring
  richtext). OpenAPI advertises `type: string` + `x-decimal-scale`; the TS SDK
  types it as `string`. Currency/unit is a companion field, not part of the type.
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
    admin-only PII, and composite `admin`-or-`owner` orders. A new schema test
    compiles every shipped example and pins farmly's access wiring.
- Authentication & authorization — composite access rules (ADR-0016): an access
  rule value may now be `{any: [rule, …]}`, a logical **OR** of sub-rules, in both
  collection `access:` and field `fields.<name>.access`.
  - Fills the gap a bare `owner` left: `any: [admin, owner]` grants admins full
    access (unfiltered list) while narrowing everyone else to the rows they
    created — whereas `owner` alone also hid records from admins.
  - Each element of `any:` is itself a rule (keyword, role, role list, or nested
    `any:`); roles named anywhere in the tree must be declared; a composite needs
    at least two sub-rules; the only supported key is `any` (an `all:`/AND form is
    intentionally deferred).
  - **No enforcement-path or query-cost change** — the evaluator folds a composite
    into the existing allow/owner-scope/deny decision (precedence
    `allow > owner-scope > deny`), so a list read still emits at most one
    `created_by` filter and issues no extra query.
  - **Reference schema** — `examples/farmly.schema.yaml` uses `any: [admin, owner]`
    for `orders.read` and `customers.update`.
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

[Unreleased]: https://github.com/blazing-Gael/dcms/compare/v0.1.0-beta.2...HEAD
[0.1.0-beta.2]: https://github.com/blazing-Gael/dcms/releases/tag/v0.1.0-beta.2
[0.1.0-beta.1]: https://github.com/blazing-Gael/dcms/releases/tag/v0.1.0-beta.1
