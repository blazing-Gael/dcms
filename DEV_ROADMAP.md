# LxCMS — Development Roadmap

Each phase has explicit acceptance criteria. A phase is complete when every criterion passes.
Do not start Phase N+1 until Phase N criteria are all green.

---

## Phase 1 — mql core + schema compiler + virtual endpoints

**Goal:** A developer writes `lxcms.schema.yaml`, runs `lxcms dev`, and gets a fully working
CRUD HTTP API with typed TypeScript output. No auth. No vectors. No admin UI. Just the core loop.

### 1.1 mql — SQLite adapter

- [ ] `mql.Adapter` interface defined in `/core/storage/mql/interface.go` exactly as
      specified in `MQL_INTERFACE.md`
- [ ] SQLite adapter at `/core/storage/sqlite/adapter.go` implementing `mql.Adapter`
- [ ] `Find` with limit, cursor (keyset), sort, fields, eq filters
- [ ] `FindOne` returns `ErrNotFound` when missing
- [ ] `Create` generates UUID v4 id, sets `created_at`/`updated_at` if schema has timestamps
- [ ] `Update` applies partial update (PATCH semantics — absent keys are not touched)
- [ ] `Delete` hard delete (soft delete in Phase 2)
- [ ] `Aggregate` — count, sum, avg with optional group_by
- [ ] `RawQuery` and `RawExec`
- [ ] `Tx` — begin/commit/rollback wrapping a `TxFunc`
- [ ] `Introspect`, `Diff`, `Migrate` — used by the schema compiler to create tables on boot
- [ ] All sentinel errors exported: `ErrNotFound`, `ErrConflict`, `ErrInvalidInput`
- [ ] WAL mode enabled on every SQLite connection open
- [ ] `$1, $2` placeholders translated to `?, ?` for SQLite internally
- [ ] Unit tests: each operation round-trips cleanly against an in-memory SQLite DB

**Acceptance test:**
```go
db, _ := sqlite.New(sqlite.Config{Path: ":memory:"})
rec, err := db.Create(ctx, mql.WriteInput{
    Collection: "products",
    Data: mql.Record{"title": "Honey", "price": 12.5},
})
assert.NoError(t, err)
assert.NotEmpty(t, rec["id"])

page, err := db.Find(ctx, mql.Query{Collection: "products", Limit: 10})
assert.Equal(t, 1, len(page.Data))
```

---

### 1.2 Schema compiler

- [ ] Parser at `/core/schema/parser.go` reads `lxcms.schema.yaml` into `SchemaDefinition`
- [ ] `SchemaDefinition` struct holds `[]CollectionDef`, each with `[]FieldDef` and directives
- [ ] Phase 1 field types parsed: `string`, `text`, `number`, `integer`, `boolean`,
      `date`, `datetime`, `enum`, `json`
- [ ] Phase 1 directives parsed: `fields`, `timestamps`, `indexes`
- [ ] Validation: field names are lowercase snake_case, enum has non-empty values,
      no reserved collection names
- [ ] Validation errors returned as a list with paths — not panics
- [ ] On startup: compiler calls `Introspect` → `Diff` → `Migrate` to create/update tables
- [ ] `CollectionDef` → `mql.CollectionMeta` translation for migration planning
- [ ] Phase 2+ directives recognised but skipped with a `// TODO(phase-2)` comment —
      no error on unknown-but-valid directives

**Acceptance test:**
```yaml
# test.schema.yaml
version: "1"
collections:
  products:
    fields:
      title: string
      price: number
      status:
        type: enum
        values: [draft, active]
    timestamps: true
    indexes: [status]
```
Parser returns `SchemaDefinition` with one collection, three fields (+ created_at, updated_at),
one index. Table is created in SQLite with correct columns.

---

### 1.3 Virtual HTTP router

- [ ] Router at `/core/gateway/router.go` using `chi`
- [ ] For each collection in schema, register these routes:
  - `GET    /api/v1/{collection}`      → `handler.List`
  - `POST   /api/v1/{collection}`      → `handler.Create`
  - `GET    /api/v1/{collection}/{id}` → `handler.GetOne`
  - `PATCH  /api/v1/{collection}/{id}` → `handler.Update`
  - `DELETE /api/v1/{collection}/{id}` → `handler.Delete`
- [ ] `GET /__schema`   → returns parsed `SchemaDefinition` as JSON
- [ ] `GET /__health`   → returns `{"status":"ok"}`
- [ ] `GET /__ready`    → returns `{"status":"ok"}` after DB ping passes
- [ ] `GET /__openapi`  → returns OpenAPI 3.1 spec generated from schema (see 1.4)
- [ ] List handler supports: `limit`, `cursor`, `sort`, `fields`, `filter[field]=value`
- [ ] All responses use the standard envelope (see SCHEMA_SPEC.md)
- [ ] 404 on unknown collection (not a panic)
- [ ] 422 on validation error with field-level messages
- [ ] 500 never exposes internal error details — logs full error, returns generic message
- [ ] Request logging middleware: method, path, status, duration, request_id

**Acceptance test (HTTP):**
```
POST /api/v1/products   {"title":"Honey","price":12.5,"status":"active"}
→ 201 {"data":{"id":"...","title":"Honey","price":12.5,"status":"active","created_at":"..."}}

GET /api/v1/products?filter[status]=active&limit=5
→ 200 {"data":[...],"meta":{"total":1,"limit":5,"next_cursor":""}}

GET /api/v1/products/nonexistent
→ 404 {"error":{"code":"NOT_FOUND","message":"record not found"}}
```

---

### 1.4 TypeScript codegen

- [ ] Codegen at `/core/schema/codegen/typescript.go` using `text/template`
- [ ] `lxcms codegen --lang ts --out ./types` generates one `.d.ts` file per collection
- [ ] Each file exports: a `Record` interface matching the schema fields,
      a `CreateInput` type (all fields, required ones non-optional),
      an `UpdateInput` type (all fields optional, id required)
- [ ] Enum fields generate a TypeScript `enum` or union type
- [ ] `datetime`/`date` fields typed as `string` (ISO 8601) — not `Date`
- [ ] `json` fields typed as `unknown`
- [ ] `timestamps: true` adds `createdAt: string` and `updatedAt: string` as readonly

**Acceptance output for Farmly products:**
```typescript
// generated — do not edit

export interface Product {
  readonly id: string;
  title: string;
  price: number;
  stock: number;
  status: ProductStatus;
  readonly createdAt: string;
  readonly updatedAt: string;
}

export type ProductStatus = 'draft' | 'active' | 'archived';

export type CreateProduct = Omit<Product, 'id' | 'createdAt' | 'updatedAt'>;
export type UpdateProduct = Partial<CreateProduct> & { id: string };
```

---

### 1.5 CLI — Phase 1 commands

- [ ] `lxcms dev [--schema path] [--port 3000] [--db path]`
  - Parses schema, runs migrations, starts HTTP server
  - Hot-reloads on schema file change (watches with `fsnotify`)
  - Defaults: schema = `./lxcms.schema.yaml`, port = `3000`, db = `./lxcms.db`
- [ ] `lxcms validate [--schema path]`
  - Parses and validates schema, prints errors, exits non-zero on failure
- [ ] `lxcms codegen --lang ts [--out ./types] [--schema path]`
  - Generates TypeScript types for all collections
- [ ] `lxcms migrate [--schema path] [--db path] [--dry-run]`
  - Runs pending migrations, prints SQL when `--dry-run`
- [ ] `lxcms version` — prints version string

---

## Phase 2 — Postgres + vector pipeline + auth + RBAC

**Goal:** Production-ready. Swap SQLite for Postgres with one config change.
Full auth, RBAC, semantic search, relation fields, draft/publish.

### 2.1 Postgres adapter

- [ ] Postgres adapter at `/core/storage/postgres/adapter.go`
- [ ] Same `mql.Adapter` interface — no changes to the interface
- [ ] pgx/v5 connection pool with configurable min/max connections
- [ ] pgvector extension support: `Adapter` embeds a `VectorDB` sub-interface
      (separate from the core `mql.Adapter` — optional capability detection at runtime)
- [ ] `Ping` checks DB connectivity, `Close` drains the pool

### 2.2 Vector pipeline

- [ ] Goroutine pool at `/core/ai/pipeline.go`
- [ ] After every `Create` and `Update`, the handler sends a `VectorJob` to a buffered channel
- [ ] Worker goroutine: flatten `vectorize` fields → call embedding model → upsert vector
- [ ] HTTP response returns to client BEFORE the goroutine starts — never block
- [ ] `GET /api/v1/search?q=...&collection=...&limit=10` — cosine similarity over pgvector
- [ ] Pluggable embedding model via config:
  ```yaml
  ai:
    embed_model: ollama          # ollama | openai | cohere | custom
    ollama_url: http://localhost:11434
    model: nomic-embed-text
  ```

### 2.3 JWT auth middleware

- [ ] `POST /api/v1/_auth/login` — returns access + refresh JWT
- [ ] `POST /api/v1/_auth/refresh` — rotates refresh token
- [ ] `POST /api/v1/_auth/logout` — invalidates refresh token
- [ ] Middleware extracts and validates JWT on every request
- [ ] Sets `ctx.userID`, `ctx.roles` for downstream handlers

### 2.4 RBAC enforcement

- [ ] Gateway middleware checks `access` rules from schema before calling handler
- [ ] `public` — pass through
- [ ] `authenticated` — require valid JWT
- [ ] `[role, ...]` — require JWT + one of the listed roles
- [ ] `owner` — require JWT + `created_by == ctx.userID`
- [ ] Field-level access: strip read-forbidden fields from responses,
      strip write-forbidden fields from incoming data before reaching handler

### 2.5 Relation fields

- [ ] `type: relation` parsed and stored as FK column in DB
- [ ] `FindOne` optionally populates related record: `?expand=category`
- [ ] `Find` optionally populates: `?expand=category,vendor`
- [ ] Cascade rules enforced at the DB layer (FK constraints where adapter supports)

### 2.6 Draft / publish

- [ ] `draft: true` adds `_status` column: `draft | published | archived`
- [ ] `GET /api/v1/{collection}` returns only `published` records for unauthenticated calls
- [ ] `POST /api/v1/{collection}/publish/:id` — transitions `draft → published`
- [ ] Middleware checks `draft` access rule separately from main CRUD access

### 2.7 i18n field resolution

- [ ] `type: i18n` stored as JSONB
- [ ] `?locale=ar` resolves to Arabic value if available, falls back through locale chain
- [ ] Accept-Language header used as fallback if no `?locale=` param

### 2.8 Audit log

- [ ] Every write operation appended to `_audit` collection:
  `{ collection, record_id, operation, user_id, diff, timestamp }`
- [ ] `GET /api/v1/_audit?collection=products&record_id=...` (admin only)

### 2.9 Webhook delivery

- [ ] Webhook config in schema (per-collection or global)
- [ ] On create/update/delete: job enqueued to in-memory queue
- [ ] Worker retries with exponential backoff (3 attempts, then dead-letter)
- [ ] Dead-letter visible at `GET /api/v1/_webhooks/dead` (admin only)

---

## Phase 3 — CLI + TypeScript SDK + media + Wasm plugins

**Goal:** `lxcms new --template ecom` scaffolds a complete project in seconds.
Plugins work. Media pipeline works. Published npm package.

### 3.1 CLI scaffold

- [ ] `lxcms new --template <name> [--dir <path>]`
  - Templates: `ecom`, `blog`, `news`, `dashboard`, `blank`
  - Generates: `lxcms.schema.yaml`, `go.mod` stubs, TypeScript types, Next.js data fetchers,
    Postgres migration SQL, README
- [ ] `lxcms build [--output ./bin/lxcms]` — compiles to single binary
- [ ] `lxcms plugin add <path>` — validates and registers a `.wasm` plugin

### 3.2 TypeScript SDK (@lxroot/cms-client)

- [ ] npm package published from `/sdk/ts`
- [ ] `lxcms({ url })` factory — works with HTTP URL or `unix://` socket path
- [ ] Auto-generated typed methods per collection from schema codegen
- [ ] `cms.{collection}.find(opts)` → typed `Page<T>`
- [ ] `cms.{collection}.findOne(id)` → typed `T`
- [ ] `cms.{collection}.create(data)` → typed `T`
- [ ] `cms.{collection}.update(id, data)` → typed `T`
- [ ] `cms.{collection}.delete(id)` → void
- [ ] `cms.search(q, opts)` → `SearchResult[]`
- [ ] Works in Node.js, browser (ES modules), and Next.js server components
- [ ] Zero dependencies in the published package — just `fetch`

### 3.3 Media pipeline

- [ ] `POST /api/v1/_media/upload` — accepts multipart, validates mime type + size
- [ ] Resizes images to configured presets (thumbnail, medium, full)
- [ ] Stores originals and resized versions in configured storage (local or S3-compatible)
- [ ] Returns `{ url, key, size, mime_type, width, height }`
- [ ] CDN URL templating: `{{ key }}` replaced with file key in configured base URL

### 3.4 Wazero plugin runtime

- [ ] Plugin host at `/core/runtime/host.go` using `wazero`
- [ ] Hot-loads `.wasm` files from `/plugins` directory on startup
- [ ] Stable host ABI v1 — functions exposed to plugins:
  - `lxcms_find(collection_ptr, query_json_ptr) → result_json_ptr`
  - `lxcms_find_one(collection_ptr, id_ptr) → record_json_ptr`
  - `lxcms_create(collection_ptr, data_json_ptr) → record_json_ptr`
  - `lxcms_update(collection_ptr, data_json_ptr) → record_json_ptr`
  - `lxcms_delete(collection_ptr, id_ptr) → ok`
  - `lxcms_log(msg_ptr)` — structured log from plugin
  - `lxcms_http_get(url_ptr, headers_json_ptr) → response_json_ptr` (if network: true)
  - `lxcms_http_post(url_ptr, body_ptr, headers_json_ptr) → response_json_ptr` (if network: true)
- [ ] Plugins declared without `permissions.network: true` cannot call HTTP functions —
      call returns error, plugin receives no crash
- [ ] Plugin manifests must declare `lxcms_min_version` — refuse to load if incompatible

---

## Phase 4 — Admin UI + Unix socket + Dashboard builder

**Goal:** Non-developers can manage content. Next.js integration is zero-config.

### 4.1 Unix socket transport

- [ ] `lxcms dev --socket /var/run/lxcms.sock` starts a Unix socket listener
- [ ] Same HTTP API, different transport — no protocol change
- [ ] TypeScript SDK detects `unix://` URL and uses Node.js `net.Socket`
- [ ] Benchmark: latency < 0.5ms p99 for a simple FindOne on localhost

### 4.2 Admin UI

- [ ] Served at `/__admin` — built as a standalone JS bundle (no framework deps)
- [ ] Reads `GET /__schema` to know all collections, fields, relations
- [ ] Auto-renders a list view and form for every collection
- [ ] List view: sortable columns, filter bar, pagination, bulk delete
- [ ] Form: input types matched to field types, validation feedback, relation pickers
- [ ] Draft/publish controls visible when `draft: true`

### 4.3 Dashboard builder

- [ ] Named dashboard configs stored in `_dashboards` collection
- [ ] `GET /__admin/dashboards/:name` renders a dashboard from its config
- [ ] Built-in widgets: `stat-card`, `table`, `chart`, `kanban`, `activity`
- [ ] Each widget has a `realtime: true` flag that opens a filtered SSE stream
- [ ] SSE endpoint: `GET /api/v1/_live/{collection}?filter[status]=active`
  - Pushes events only for records matching the filter
  - RBAC enforced per connection — checked on connect and on every event push

---

## Phase 5 — Scale + multiplayer + enterprise features

**Goal:** Multi-node, collaborative editing, MCP protocol, embedded library mode.

- [ ] Couchbase mql adapter
- [ ] CRDT/Yjs sync hub for collaborative editing (gorilla/websocket + server-authoritative merge)
- [ ] MicroVM cluster mode (multiple lxcms instances behind a load balancer with shared Postgres)
- [ ] MCP server built-in — every collection exposed as a typed callable tool
- [ ] SAML/SSO auth provider
- [ ] CGO/FFI embedded library (`.so`/`.dll`) with N-API wrapper for Node.js
- [ ] Compliance export: GDPR data export, audit log export
- [ ] White-label admin UI
- [ ] LxRoot one-click deploy integration (`lxcms deploy --target lxroot`)

---

## Testing strategy

### Unit tests
Every package has `_test.go` files testing the package in isolation.
Use in-memory SQLite for storage tests — never mock the DB adapter.
Run with `go test ./...`

### Integration tests
`/tests/integration/` contains end-to-end HTTP tests.
Spin up a real `lxcms dev` server against a temp SQLite file.
Use `httptest.NewServer` to avoid real port binding.
Run with `go test ./tests/integration/...`

### Schema fixture tests
`/tests/schemas/` contains `.yaml` files that are either valid or invalid.
Parser tests assert each valid schema parses without error and each invalid one
returns the expected validation error message.

### Acceptance criteria format
Each phase section above lists checkboxes. A phase is "done" when:
1. All checkboxes are ticked
2. `go test ./...` passes with no failures
3. The acceptance test at the end of each section passes
4. No TODO(phase-1) markers remain in files committed to the phase-1 branch

---

## Git branch strategy

```
main              ← always releasable, tagged versions only
dev               ← integration branch, all feature branches merge here
phase-1           ← Phase 1 work branch → merges to dev when criteria met
phase-2           ← Phase 2 work branch → merges to dev when criteria met
...
feat/schema-parser     ← individual feature branches off phase-N
feat/sqlite-adapter
feat/virtual-router
```
