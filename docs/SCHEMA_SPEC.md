# LxCMS Schema Specification

Version: 0.1 (Phase 1 subset marked clearly)

The schema file (`dcms.schema.yaml`) is the single source of truth for a DCMS project.
Everything — HTTP endpoints, database migrations, TypeScript types, OpenAPI spec, admin UI
widgets, and dashboard layouts — is derived from this file automatically.

**Rule:** if it is not in the schema, it does not exist in the API.

---

## File structure

```yaml
version: "1"          # schema format version, always "1" for now

meta:                 # optional project metadata
  name: string
  description: string
  base_url: string    # default: /api/v1

brand:                # optional brand identity block (served in agent/MCP responses)
  ...

collections:          # required — at least one collection
  <name>:
    ...

auth:                 # optional auth configuration
  ...

plugins:              # optional plugin declarations
  ...
```

---

## Collections

A collection maps to a database table and a set of virtual HTTP endpoints.

```yaml
collections:
  products:                     # collection name — lowercase, snake_case
    fields:                     # required
      <name>: <field definition>
    indexes: []                 # optional — field names to index
    vectorize: []               # optional — field names to embed for semantic search
    timestamps: true            # optional — auto-add createdAt / updatedAt
    soft_delete: false          # optional — DELETE trashes (reversible) instead of removing
    publishing: false           # optional — enable draft / published / scheduled / archived
    revisions: false            # optional — keep full-snapshot version history per record
    i18n: []                    # optional — list of supported locale codes e.g. [en, ar, bn]
    access:                     # optional — RBAC rules (Phase 2+)
      ...
    hooks:                      # optional — lifecycle hook declarations
      ...
    schedule:                   # optional — cron jobs scoped to this collection (Phase 2+)
      ...
```

### Collection naming rules

- Lowercase letters, digits, underscores only
- Must start with a letter
- No reserved names: `_schema`, `_dashboards`, `_users`, `_roles`, `_audit`, `_jobs`
- Plural by convention: `products`, not `product`

---

## Field definitions

### Shorthand (Phase 1)

```yaml
fields:
  title: string          # type only, no options
  price: number
  stock: integer
```

### Full form

```yaml
fields:
  title:
    type: string         # required
    required: true       # default: false
    default: null        # default value (must match type)
    unique: false        # default: false — adds a unique constraint
    min: null            # string: min length, number/integer: min value
    max: null            # string: max length, number/integer: max value
    pattern: null        # string: regex pattern for validation
    label: "Title"       # human-readable label for admin UI
    hint: ""             # helper text shown in admin UI forms
```

### Field types

#### Scalar types (Phase 1)

| Type      | Go type     | DB type              | Notes                                      |
|-----------|-------------|----------------------|--------------------------------------------|
| `string`  | `string`    | VARCHAR(255)         | Short text                                 |
| `text`    | `string`    | TEXT                 | Long-form content                          |
| `number`  | `float64`   | NUMERIC(12,4)        | Approximate / float — **not** for money    |
| `integer` | `int64`     | BIGINT               | Whole number                               |
| `decimal` | `string`    | INTEGER minor units (SQLite) / NUMERIC(p,s) (Postgres) | Exact fixed-point money — see below |
| `boolean` | `bool`      | BOOLEAN              | true / false                               |
| `date`    | `time.Time` | DATE                 | ISO 8601 date only                         |
| `datetime`| `time.Time` | TIMESTAMPTZ          | ISO 8601 datetime with timezone            |
| `json`    | `any`       | JSONB (Postgres) / TEXT (SQLite) | Arbitrary JSON             |

#### Enum (Phase 1)

```yaml
status:
  type: enum
  values: [draft, active, archived]   # required — non-empty list of strings
  default: draft
```

Stored as VARCHAR. Validated on write. Admin UI renders as a select.

#### Decimal — exact money (ADR-0017)

```yaml
price:  { type: decimal, scale: 2 }   # money — two fractional digits (the default)
weight: { type: decimal, scale: 3 }   # any exact fixed-point quantity
cost:   decimal                        # shorthand → scale 2
```

`decimal` is an **exact** fixed-point number — use it for any value where
floating-point drift is unacceptable (prices, taxes, totals). `number` is
IEEE-754 and will accumulate rounding error (`0.1 + 0.2 != 0.3`); never store
money in it.

- **`scale`** is the fixed number of fractional digits (0–9, default **2**):
  `2` for USD/EUR, `0` for JPY, `3` for BHD/KWD, `8` for most crypto.
- **On the wire a decimal is a quoted string**, never a JSON number — `"12.50"`,
  `"-3.00"`, `"0.001"` — and is always returned at the full declared scale. A
  bare JSON number is **rejected** (it is already a lossy float by the time it
  arrives), as is a value with more fractional digits than the scale (money is
  never silently rounded). A `default` is likewise a decimal string.
- **Storage** is an exact integer count of minor units (value × 10^scale), so
  sorting, range filters (`filter[price][gte]=10.00`), and `SUM` are exact.
- **Currency/unit is a companion field, not part of the type** — pair the amount
  with an `enum` or a relation:

  ```yaml
  amount:   { type: decimal, scale: 2 }
  currency: { type: enum, values: [USD, EUR, JPY] }
  ```

  Conversion, exchange rates, and mixed-currency arithmetic are application
  concerns; DCMS guarantees each amount is stored and returned exactly.

#### Relation (Phase 2)

```yaml
author:
  type: relation
  target: users                       # target collection name — must exist in schema
  many: false                         # false = belongs-to (FK column), true = many-to-many (join table)
  on_delete: restrict                 # belongs-to only: restrict (default) | cascade | set null
  # on_delete options (what happens to this record when its target is deleted):
  #   restrict — block the target's deletion while references exist (default) → 409
  #   cascade  — delete this record too
  #   set null — clear this record's reference (requires the relation be nullable)
```

Referential integrity is enforced in two layers (see ADR-0010): the gateway
validates every referenced id exists before a write (field-named `422` on a
miss), and the database carries a real foreign key as the backstop. A
many-to-many relation's join rows always cascade when either endpoint is deleted
(this is engine-managed and not configurable); `on_delete` therefore applies only
to belongs-to relations.

#### File / media (Phase 2)

```yaml
image:   { type: file }              # a single asset (belongs-to the media library)
gallery: { type: file, many: true }  # a set of assets (a gallery)
```

A `file` field is sugar for a relation to the engine-managed `_media` collection
(ADR-0011): it stores a media asset's id, expands to the media object (with a
`url`), and honors `on_delete` like any belongs-to relation (default `restrict`,
so an asset still in use can't be deleted). Assets are uploaded and managed
through the `/__media` endpoints — you reference them by id — and the media
library is browsable, replaceable, and answers "where is this used?" via reverse
expansion. Do not set `target` on a file field; it is always `_media`.

#### Rich content — `richtext` (ADR-0014)

Formatted body content: headings, bold/italic/links, lists, and inline embeds
that reference media and other records. The value is a **structured document**
(a portable-text-style JSON array of nodes), stored in a JSON column — never HTML
or a markdown string — so it renders to any target (HTML, React, plain text) and
its embeds are first-class references, not brittle inline markup.

```yaml
body:
  type: richtext
  styles: [normal, h2, h3, blockquote]   # allowed block styles (labels; free-form)
  marks:  [strong, em, code, link]       # allowed decorators + annotation types
  blocks: [image, reference]             # allowed custom (non-text) block types
```

All three lists are optional; omitted, they default to
`styles: [normal, h1, h2, h3, h4, blockquote]`, `marks: [strong, em, code, link]`,
`blocks: [image]`. `marks` must be known decorators (`strong`, `em`, `code`,
`underline`, `strike`) or annotations (`link`, `reference`); `blocks` must be known
types (`image`, `code`, `embed`, `reference`) — an unknown one fails schema
validation. `styles` are free-form labels a renderer maps.

The stored document is an array of nodes:

```jsonc
[
  { "_type": "block", "style": "h2",
    "children": [ { "_type": "span", "text": "Hello", "marks": ["strong"] } ] },
  { "_type": "block", "style": "normal",
    "markDefs": [ { "_key": "l1", "_type": "link", "href": "https://example.com" } ],
    "children": [ { "_type": "span", "text": "a link", "marks": ["l1"] } ] },
  { "_type": "image", "ref": "<media id>", "alt": "a photo" },        // → _media
  { "_type": "reference", "collection": "authors", "ref": "<id>" }    // → a record
]
```

On write the document is validated structurally (well-formed nodes, styles/marks/
blocks within the field's allowlists, spans whose marks resolve to a decorator or
a declared `markDef`, safe link schemes — no `javascript:`), and every in-content
reference (image → `_media`, `reference` nodes → the named collection) is checked
for existence in the same batched pass as relations (field-named `422` on a
dangling ref; no N+1). Because references live inside a JSON blob there is no DB
foreign key protecting them — the validation layer owns their correctness.

`?expand=body` resolves the referenced records (image blocks → their `_media`
asset with a `url`, `reference` nodes → the named record) into a **deduplicated
`included` manifest at the response root**, keyed `"<collection>:<id>"`; the
document AST stays id-only (ADR-0015). This keeps documents cacheable, serializes a
shared reference once across a list, and makes reference cycles harmless. Like
relation expansion it is batched (one query per target collection, no N+1) and
lifecycle-aware (a hidden or missing target is simply absent from `included`).

#### i18n (Phase 2)

```yaml
title:
  type: i18n
  required: true
  # stored as JSONB: { "en": "...", "ar": "...", "bn": "..." }
  # resolved to a single string on read via ?locale= param or Accept-Language header
  # falls back through locale chain defined in collection.i18n
```

#### Media (Phase 3)

```yaml
images:
  type: media
  multiple: false      # true = array of media refs
  accept: [image/jpeg, image/png, image/webp]
  max_size_mb: 10
  # stored as media ref object: { url, key, size, mime_type, width, height }
  # url is a CDN URL resolved at read time
```

#### Geo (Phase 3)

```yaml
location:
  type: geo
  # stored as { lat: float64, lng: float64 }
  # enables map widget in dashboard automatically
```

#### Computed (Phase 3)

```yaml
sale_price:
  type: computed
  expr: "price * (1 - discount)"     # simple arithmetic expression over sibling fields
  # never stored — evaluated at read time
  # available field names in expr: any non-computed field in the same collection
```

---

## Timestamps

```yaml
timestamps: true
# Adds two fields automatically:
#   created_at: datetime (set on create, never updated)
#   updated_at: datetime (set on create and every update)
# These fields cannot be set by clients — they are managed by the engine.
```

---

## Publishing / draft state machine (ADR-0012)

```yaml
publishing: true
# Adds engine-managed columns:
#   _status:       draft | published | archived   (default draft, readonly)
#   _published_at: datetime, nullable             (the go-live instant)
#
# Public reads show only LIVE content: _status = published AND _published_at <= now.
# A FUTURE _published_at schedules a go-live — the record becomes visible the
# instant the clock passes it, with no background job. `archived` is retired-but-
# kept: hidden from the public and from the drafts view, but preserved.
#
# Transition endpoints (a record starts as draft):
#   POST /api/v1/<collection>/:id/publish     {"at"?: "<RFC3339>"}  → published (or scheduled)
#   POST /api/v1/<collection>/:id/unpublish                          → draft
#   POST /api/v1/<collection>/:id/archive                            → archived
#
# Admin/preview (see the preview token below) may widen the view with
#   ?status=draft | published | scheduled | archived | any
```

---

## Indexes

```yaml
indexes: [status, created_at]
# Creates database indexes on the listed fields.
# Composite indexes:
indexes:
  - [category, status]    # composite index on (category, status)
  - created_at            # single-column index
```

---

## Vectorize (Phase 2)

```yaml
vectorize: [title, description]
# After every create or update, a background goroutine:
#   1. Flattens the listed fields to a plain text string
#   2. Calls the configured embedding model
#   3. Stores the vector in a pgvector column
# Never blocks the HTTP response.
# Enables: GET /api/v1/search?q=...&collection=<name>
```

---

## Soft delete (ADR-0012)

```yaml
soft_delete: true
# Adds an engine-managed _deleted_at (datetime, nullable, readonly).
#   DELETE /api/v1/<collection>/:id            trashes the row (sets _deleted_at)
#   POST   /api/v1/<collection>/:id/restore    undeletes it
#   DELETE /api/v1/<collection>/:id?purge=true permanently removes it (still
#                                              honors on_delete: restrict → 409)
# Reads exclude trashed records automatically. Admin/preview may include them via
#   ?include_deleted=true   (active + trashed)
#   ?include_deleted=only   (trash view)
```

## Preview token (ADR-0012)

The `?status` and `?include_deleted` params above are honored **only** for a
request carrying a valid preview token — the `X-DCMS-Preview` header (or a
`preview_token` query param) matching the server's `DCMS_PREVIEW_TOKEN` (env-only;
it is a secret and is never read from the config file). Without the token these
params are ignored and the public view (live, non-trashed) is returned. A hidden
record is answered with `404`, never `403`, so its existence doesn't leak.

## Revisions / version history (ADR-0013)

```yaml
revisions: true
# Keeps a full-snapshot version history of every record in the collection. On each
# write DCMS captures the whole record as JSON in the same transaction (history can
# never diverge from the record), labeled with the operation and a per-record
# incrementing version:
#   create / update / publish / unpublish / archive / restore / delete
#
#   GET  /api/v1/<collection>/:id/revisions                  history (newest first,
#                                                            no snapshot blobs)
#   GET  /api/v1/<collection>/:id/revisions/:version         one version + snapshot
#   POST /api/v1/<collection>/:id/revisions/:version/restore roll content back
#
# Restore is content-only: it restores declared fields but leaves the managed
# lifecycle columns (_status/_published_at/_deleted_at) as they are, and is itself
# recorded as a new revision (history is append-only).
#
# History endpoints are gated by the same preview token as above (a request without
# it gets 404). Snapshots live in an engine-managed `_revisions` collection that is
# added only when at least one collection opts in.
```

---

## Access control (collection-level rules are LIVE — ADR-0016)

```yaml
access:
  read:    public              # anyone, no auth required
  create:  [admin, vendor]     # authenticated users with one of these roles
  update:                      # admins OR the record's creator (composite)
    any: [admin, owner]
  delete:  [admin]
  # rule values:
  #   public         — no authentication required
  #   authenticated  — any valid principal, regardless of role
  #   [role, ...]    — the principal holds at least one listed role
  #   owner          — the principal is the record's created_by
  #   {any: [...]}   — composite OR: satisfied if any listed sub-rule is
```

The `any:` composite lets one rule mix gates that resolve differently per caller.
`any: [admin, owner]` is the canonical case: an admin gets full access (list is
unfiltered), while everyone else is narrowed to the rows they created — exactly
what a bare `owner` could **not** express, since `owner` alone also hides records
from admins. Each element of `any:` is itself a rule (keyword, role, role list,
or nested `any:`), roles named anywhere in it must be declared, and it needs at
least two sub-rules. There is no query-cost or `N+1` change: an admin resolves to
a plain allow, a non-owner to the same single `created_by` filter as `owner`.

Enforcement (ADR-0016) is at the gateway, above the store:

- A **denied write** (create/update/delete/transition) → `403` (or `401` if the
  caller is anonymous). A **denied single read** → `404`, never `403`, so a
  record's existence never leaks. `owner` on a **list** read narrows the query to
  the caller's own rows rather than forbidding the endpoint.
- **Default when `access:` is omitted:** reads are `public`, writes are
  `authenticated`. Tightening is additive, per collection.
- Roles named in a rule must be declared under `auth.roles` (a typo is a
  schema-compile error). There is no role hierarchy — list every role that passes.

Field-level access (LIVE — ADR-0016):

```yaml
fields:
  internal_notes:
    type: text
    access:
      read: [admin]     # hidden from non-admins in all responses
      write: [admin]    # ignored if submitted by non-admins
```

- Uses the same rule grammar as collection access
  (`public` | `authenticated` | `[role, …]` | `owner` | `{any: [...]}`); an omitted
  direction is `public` (the collection rule is the real gate).
- **`read` is a mask, not a gate:** an unauthorized reader still receives the
  record — just without that field (in single, list, and `?expand`ed responses).
- **`write` is a filter, not a rejection:** an unauthorized writer's value for the
  field is silently dropped, so a client that round-trips a record it read never
  gets a `4xx` for a field it was never allowed to see. On **create**, an `owner`
  write rule collapses to `authenticated` (you become the owner of what you make).
- Only `read`/`write` are valid keys here (the CRUD verbs belong to the collection);
  any other key is a schema-compile error, as is a role not declared in `auth.roles`.

---

## Hooks (Phase 2)

```yaml
hooks:
  before_create: validate-inventory    # plugin name or inline handler ref
  after_create:  notify-new-order
  before_update: null
  after_update:  reindex-search
  before_delete: check-dependencies
  after_delete:  null
# Hook values are plugin names registered in the plugins section.
# Hooks receive the full record and can return a modified version (before_*) or void (after_*).
# before_* hooks can abort the operation by returning an error.
```

---

## Scheduled jobs (Phase 2)

```yaml
schedule:
  - name: expire-flash-sales
    cron: "0 * * * *"          # every hour
    handler: expire-pricing     # plugin name
  - name: reindex-all
    cron: "0 2 * * *"          # 2am daily
    handler: full-reindex
```

---

## Brand identity block

Served alongside data in agent/MCP responses. Lets AI clients render consistently with
the brand without serving HTML.

```yaml
brand:
  name: "Farmly"
  tagline: "Farm to table, direct."
  colors:
    primary: "#2D6A4F"
    accent: "#52B788"
    background: "#F9FAF7"
    text: "#1A1A1A"
  fonts:
    heading: "Fraunces"
    body: "Inter"
  logo_url: "https://cdn.farmly.com/logo.svg"
  tone: "warm, honest, local"       # free text — used by AI to match brand voice
  rtl: false                        # true for Arabic/Hebrew/etc.
  locale: en                        # default locale
```

---

## Auth configuration (local provider is LIVE — ADR-0016)

```yaml
auth:
  provider: local                   # local | oidc | both (default local)
  session:                          # local provider: opaque DB-backed sessions
    ttl: 168h                       # Go duration; default 7 days
  roles:                            # role definitions (referenced by access rules)
    admin:
      label: "Administrator"
    vendor:
      label: "Vendor"
    customer:
      label: "Customer"

  # ── Reserved for the external-identity milestone (not yet enforced) ──
  jwt:                              # applies to the EXTERNAL provider only
    algorithm: HS256                # HS256 | RS256
    secret: ${JWT_SECRET}
  oidc:                            # provider: oidc | both
    issuer: "https://accounts.google.com"
    client_id: ${OIDC_CLIENT_ID}
    client_secret: ${OIDC_CLIENT_SECRET}
```

**Local identity uses opaque, DB-backed sessions, not JWT** (ADR-0016). Login at
`POST /auth/login` exchanges `{email, password}` for a session token (returned in
the body and as an `HttpOnly` cookie); send it back as `Authorization: Bearer
<token>` or the cookie. `POST /auth/logout` revokes it immediately; `GET /auth/me`
returns the current principal. The `jwt:` block above pertains to the *external*
OIDC provider (a later milestone), where DCMS verifies tokens issued by the IdP.

Two engine-managed collections back this: `_users` (email, password_hash, roles)
and `_sessions` — both reserved and not JSON-CRUD routable. `password_hash` is
never serialized in any response.

**Bootstrap:** create the first admin with `dcms admin create --email … --password
…`, or set `DCMS_ADMIN_EMAIL` / `DCMS_ADMIN_PASSWORD` (env-only secrets) and the
server seeds an admin on first run when no users exist yet.

---

## Plugin declarations (Phase 3)

```yaml
plugins:
  notify-new-order:
    path: ./plugins/notify-new-order.wasm
    version: "1.0.0"
    permissions:
      network: true             # allow outbound HTTP (for webhooks, email)
      env: [SMTP_HOST, SMTP_PORT, SMTP_USER, SMTP_PASS]
  expire-pricing:
    path: ./plugins/expire-pricing.wasm
    version: "1.2.0"
    permissions:
      collections: [products]   # read/write access to these collections
```

---

## Auto-generated endpoints

For every collection, LxCMS generates these endpoints automatically:

```
GET    /api/v1/<collection>           list (paginated, filterable, sorted)
POST   /api/v1/<collection>           create
GET    /api/v1/<collection>/:id       get one
PATCH  /api/v1/<collection>/:id       update (partial)
DELETE /api/v1/<collection>/:id       delete

# With draft: true
POST   /api/v1/<collection>/publish/:id
POST   /api/v1/<collection>/archive/:id
POST   /api/v1/<collection>/unpublish/:id

# With vectorize (Phase 2)
GET    /api/v1/search?q=...&collection=<name>&limit=10

# Aggregation (Phase 2)
GET    /api/v1/<collection>/aggregate?metric=count|sum|avg&field=<name>&group_by=<name>

# Introspection (always available)
GET    /__schema                      live schema as structured JSON
GET    /__health                      health probe
GET    /__ready                       readiness probe
GET    /__openapi                     OpenAPI 3.1 spec generated from schema
```

### List query parameters

```
?limit=20           page size, default 20, max 100
?cursor=<token>     cursor for next page (returned in response as next_cursor)
?sort=created_at    field to sort by, prefix with - for descending (?sort=-created_at)
?fields=id,title    sparse fieldset — only return these fields
?locale=ar          resolve i18n fields to this locale
?q=<text>           full-text search (basic) — Phase 1, vector search Phase 2
```

### Filtering

```
?filter[status]=active
?filter[price][gte]=100
?filter[price][lte]=500
?filter[title][contains]=honey
?filter[created_at][gte]=2024-01-01

# Operators: eq (default) | ne | gt | gte | lt | lte | contains | starts_with | in | nin
# Multiple filters are ANDed together.
```

---

## Response envelope

All responses use a consistent envelope:

```json
// Success — single record
{
  "data": { "id": "...", ... },
  "meta": {}
}

// Success — list
{
  "data": [ ... ],
  "meta": {
    "total": 142,
    "limit": 20,
    "next_cursor": "eyJpZCI6IjEyMyJ9"
  }
}

// Error
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "title is required",
    "fields": { "title": "required" }
  }
}
```

Error codes:
```
VALIDATION_ERROR    — field validation failed
NOT_FOUND           — record does not exist
UNAUTHORIZED        — missing or invalid JWT
FORBIDDEN           — authenticated but insufficient role
CONFLICT            — unique constraint violation
RATE_LIMITED        — per-key rate limit exceeded
INTERNAL            — unexpected server error (never expose details in production)
```

---

## Environment variable references

Any string value in the schema can reference an environment variable:

```yaml
jwt:
  secret: ${JWT_SECRET}
```

LxCMS resolves `${VAR_NAME}` at startup. If the variable is not set and the field is required,
startup fails with a clear error message listing the missing variables.

---

## Schema validation

DCMS validates the schema on startup and on `dcms dev` reload:

- All `relation` targets must exist as collections in the same schema
- All `vectorize` fields must be of type `string`, `text`, or `i18n`
- All `computed` expressions must reference only non-computed fields in the same collection
- All `hooks` values must reference a declared plugin
- Enum `values` must be non-empty and contain no duplicates
- Field names must be lowercase snake_case and not start with `_`
- Collection names follow the same rules

Validation errors are reported as a list with field paths:
```
schema validation failed:
  collections.products.fields.category: relation target "categories" not found
  collections.orders.fields.status: enum values contains duplicate "active"
```

---

## Phase 1 subset

For Phase 1, implement only:

**Supported field types:** `string`, `text`, `number`, `integer`, `decimal`, `boolean`, `date`, `datetime`, `enum`, `json`, `richtext`, `relation`, `file`

**Supported collection directives:** `fields`, `timestamps`, `indexes`,
`publishing`, `soft_delete`, `revisions`

**Supported endpoints:** list, create, get one, update, delete; plus (per
directive) publish / unpublish / archive / restore transitions and
revisions list / get / restore

**Query params:** `limit`, `cursor`, `sort`, `fields`, `filter` (eq only)

Everything else in this spec is the target — implement incrementally per `DEV_ROADMAP.md`.
Mark unimplemented features with a `// TODO(phase-N):` comment in the parser.
