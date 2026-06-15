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
    soft_delete: false          # optional — mark as deleted, don't hard remove
    draft: false                # optional — enable draft/publish/archive state machine
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
| `number`  | `float64`   | NUMERIC(12,4)        | Decimal / float                            |
| `integer` | `int64`     | BIGINT               | Whole number                               |
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

#### Relation (Phase 2)

```yaml
category:
  type: relation
  collection: categories              # target collection name — must exist in schema
  many: false                         # false = belongs-to (FK), true = has-many (join table)
  cascade: nullify                    # nullify | restrict | cascade
  # cascade options:
  #   nullify  — set FK to NULL when target is deleted (default)
  #   restrict — prevent deletion of target if references exist
  #   cascade  — delete this record when target is deleted
```

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

## Draft / publish state machine

```yaml
draft: true
# Adds a hidden _status field with values: draft | published | archived
# Endpoints:
#   GET /api/v1/<collection>          returns only published records (public)
#   GET /api/v1/<collection>?all=true returns all records (requires auth + role)
#   POST /api/v1/<collection>/publish/:id   transitions draft → published
#   POST /api/v1/<collection>/archive/:id   transitions published → archived
#   POST /api/v1/<collection>/unpublish/:id transitions published → draft
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

## Soft delete

```yaml
soft_delete: true
# DELETE /api/v1/<collection>/:id sets _deleted_at = now() instead of removing the row.
# GET /api/v1/<collection> excludes soft-deleted records automatically.
# GET /api/v1/<collection>?include_deleted=true includes them (requires auth).
# Hard delete: DELETE /api/v1/<collection>/:id?hard=true (requires admin role).
```

---

## Access control (Phase 2)

```yaml
access:
  read:    public              # anyone, no auth required
  create:  [admin, vendor]     # authenticated users with these roles
  update:  [admin, vendor]
  delete:  [admin]
  # special values:
  #   public      — no authentication required
  #   authenticated — any valid JWT, regardless of role
  #   [role, ...]  — one of the listed roles
  #   owner       — the user who created the record (matches created_by field)
```

Field-level access (Phase 2):

```yaml
fields:
  internal_notes:
    type: text
    access:
      read: [admin]     # hidden from non-admins in all responses
      write: [admin]    # ignored if submitted by non-admins
```

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

## Auth configuration (Phase 2)

```yaml
auth:
  provider: local                   # local | oidc | both
  jwt:
    algorithm: HS256                # HS256 | RS256
    secret: ${JWT_SECRET}           # env var reference
    access_ttl: 15m
    refresh_ttl: 7d
  roles:                            # role definitions
    admin:
      label: "Administrator"
    vendor:
      label: "Vendor"
    customer:
      label: "Customer"
  # OIDC config (if provider: oidc or both):
  oidc:
    issuer: "https://accounts.google.com"
    client_id: ${OIDC_CLIENT_ID}
    client_secret: ${OIDC_CLIENT_SECRET}
```

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

**Supported field types:** `string`, `text`, `number`, `integer`, `boolean`, `date`, `datetime`, `enum`, `json`

**Supported collection directives:** `fields`, `timestamps`, `indexes`

**Supported endpoints:** list, create, get one, update, delete

**Query params:** `limit`, `cursor`, `sort`, `fields`, `filter` (eq only)

Everything else in this spec is the target — implement incrementally per `DEV_ROADMAP.md`.
Mark unimplemented features with a `// TODO(phase-N):` comment in the parser.
