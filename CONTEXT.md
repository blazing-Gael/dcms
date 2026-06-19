# DCMS — Project Context

## What this is

DCMS is a schema-first, AI-native, headless content operating system written in Go.
It is sovereign by design — self-hostable on your own infrastructure, no platform lock-in.

The core insight: every existing CMS forces a choice between managed-and-locked-in (Shopify, Sanity)
or flexible-and-build-it-yourself (Strapi, raw Postgres). DCMS fills the gap.

**One-line pitch:** Define your data model in YAML. Get a production backend, typed SDKs,
admin UI, semantic search, and one-click deploy — without writing a line of Go.

---

## Guiding principles

1. **Schema is the single source of truth.** Everything — endpoints, migrations, TypeScript types,
   admin UI, OpenAPI spec — derives from `dcms.schema.yaml` automatically.

2. **Developers never touch Go.** The engine is Go. Developers use YAML, the CLI, SDKs, REST,
   and Wasm plugins. Go knowledge is never required.

3. **Headless by design.** Not retrofitted. The API layer is the only layer.

4. **AI-first.** Schema introspection endpoint, typed codegen, and MCP protocol support
   are first-class primitives, not plugins.

5. **Sovereign.** Runs on a $5 VPS. Deploys to your own infra. No platform fees, no vendor SLA required.

6. **Performance ceiling.** Single Go binary. Low memory. Sub-millisecond Unix socket IPC.
   pgvector inline. No PHP-FPM, no Varnish, no Solr.

---

## Product tiers

```
Tier 1 — Open Source Core (this repo, MIT)
  schema engine · store layer · vector pipeline · Wasm runtime · RBAC · i18n
  draft/publish · audit log · dashboard builder · CLI · SDKs · web components

Tier 2 — AI Builder (paid, managed hosting)
  brief → schema → deploy · UI generation · curated plugin library · managed hosting

Tier 3 — Enterprise (contract)
  cluster mode · Couchbase adapter · CRDT sync · MCP server · SSO · custom SLAs
```

Community can contribute to Tier 1 and publish plugins to the registry.
Tier 2 and 3 features are closed-source and build on the open core.

---

## Architecture overview

### Deployment modes

- **Mode A — Sidecar (default, ship first):** DCMS binary runs alongside the app on the same
  server. Communication over Unix domain socket. Sub-millisecond overhead.

- **Mode B — Standalone cluster:** HTTP/3 + gRPC binary, isolated process or MicroVM,
  serves multiple frontend nodes over the network.

- **Mode C — Embedded library (Phase 5+):** Compiles to `.so`/`.dll` via CGO/N-API.
  In-process, zero latency. Only after sidecar mode is production-proven.

### Core packages

```
/core/schema      YAML/JSON parser → CollectionDef structs → virtual router → OpenAPI + TS codegen
/core/store       store layer: repository interface + SQLite/Postgres/Couchbase adapters
/core/ai          Vector ingestion goroutine, embedding pipeline, semantic search
/core/gateway     JWT auth, RBAC enforcement, per-key rate limiting, request logging
/core/media       Upload → resize → CDN URL pipeline
/core/sync        WebSocket hub + CRDT state sync (Phase 5)
/core/runtime     Wazero Wasm plugin sandbox
/bindings/cgo     CGO export layer (Phase 5)
/bindings/node    N-API wrapper (Phase 5)
/sdk/ts           TypeScript SDK + codegen (@dcms/client)
/sdk/flutter      Dart SDK
/sdk/python       Async Python client
/cmd/dcms        CLI entrypoint
```

### Storage adapters (store)

| Adapter    | Profile              | Notes                                      |
|------------|----------------------|--------------------------------------------|
| SQLite     | Dev default          | Zero-dependency, fast local iteration      |
| PostgreSQL | Production default   | pgvector, ACID, full-text, migrations      |
| Couchbase  | Enterprise           | Sub-document ops, high-write, SQL++        |

Adapters are swapped via one config line. The store interface is stable from Phase 1.
**Do not change the store interface after Phase 1 without a major version bump.**

---

## Directory layout

```
dcms/
├── README.md
├── CONTEXT.md              ← you are here (architecture overview)
├── ROADMAP.md              ← outward-facing direction
├── CHANGELOG.md
├── LICENSE  CONTRIBUTING.md  CODE_OF_CONDUCT.md  SECURITY.md
├── go.mod  go.sum  Makefile
├── .github/                ← CI workflow, issue/PR templates
├── docs/
│   ├── SCHEMA_SPEC.md      ← full schema language reference
│   ├── STORE_INTERFACE.md  ← store Go interface contract
│   ├── DEV_ROADMAP.md      ← phased build plan with acceptance criteria
│   └── adr/                ← architecture decision records (the "why")
├── examples/
│   └── farmly.schema.yaml  ← real-world e-commerce schema (reference)
├── cmd/
│   └── dcms/               ← CLI entrypoint (dev, validate, migrate)
├── core/
│   ├── schema/             ← parser, validation, OpenAPI, codegen
│   ├── store/              ← storage interface + sqlite adapter (postgres, couchbase later)
│   ├── gateway/            ← virtual HTTP router, validation, docs
│   ├── engine/             ← composition: load · migrate · serve
│   ├── ai/                 ← vector pipeline (Phase 2)
│   ├── media/              ← upload pipeline (Phase 3)
│   ├── sync/               ← CRDT hub (Phase 5)
│   └── runtime/            ← Wasm plugin sandbox (Phase 3)
├── bindings/               ← cgo / node (Phase 5)
└── sdk/                    ← ts / flutter / python
```

---

## Key constraints Claude Code must respect

1. **store interface is locked after Phase 1.** Every adapter implements it. Never add methods
   that break the interface without a major version. See `docs/STORE_INTERFACE.md`.

2. **HTTP response is never blocked by the embed goroutine.** The vector pipeline fires
   after write commit in a background goroutine. The client receives the response immediately.

3. **No schema drift between YAML and generated types.** TypeScript types, OpenAPI spec,
   and admin UI widgets are all generated from the same parsed schema structs. Never hardcode
   field names in generated output.

4. **Transactions from day one.** Every store adapter must implement `Tx()`. Even if
   a feature doesn't use transactions yet, the interface must be there. Oversell bugs
   from missing atomicity are silent and hard to debug.

5. **Plugin ABI is stable from Phase 4.** Once the Wasm host API is defined, treat it
   like a public API. Plugins must declare a minimum DCMS version. Breaking changes
   require a new ABI version, not a silent break.

6. **RBAC is enforced at the gateway layer, not in collection handlers.** Collection
   handlers assume the caller is authorized. The gateway middleware does the check.
   This keeps authorization logic in one place.

7. **i18n fields return the requested locale.** If `?locale=ar` is passed and the field
   has an `ar` value, return it. If not, fall back through the locale chain defined in schema.
   Never return a raw locale map to the client — always resolve to a single value.

---

## Tech stack decisions

| Concern           | Decision                  | Reason                                        |
|-------------------|---------------------------|-----------------------------------------------|
| Language          | Go 1.22+                  | Single binary, low memory, fast concurrency   |
| Router            | `chi`                     | Lightweight, idiomatic, middleware-friendly   |
| SQLite driver     | `mattn/go-sqlite3`        | Mature, CGO, full feature support             |
| Postgres driver   | `pgx/v5`                  | Native, fast, pgvector support                |
| Wasm runtime      | `wazero`                  | Pure Go, zero native deps, sandboxed          |
| Config parsing    | `gopkg.in/yaml.v3`        | Schema is YAML-first                          |
| JWT               | `golang-jwt/jwt/v5`       | RS256 + HS256 support                         |
| CLI               | `cobra`                   | Standard, flag parsing, subcommands           |
| Testing           | stdlib `testing` + testify | No magic, fast feedback                      |
| TS codegen        | Text templates (`text/template`) | No external codegen dep             |

---

## What "done" looks like for Phase 1

A developer can:
1. Write a `dcms.schema.yaml` with two collections (products, stories)
2. Run `dcms dev` and get a working HTTP server on `localhost:3000`
3. `POST /api/v1/products` with a JSON body and get a `201` back
4. `GET /api/v1/products` and get a paginated JSON list back
5. `GET /api/v1/products/:id` and get a single record back
6. `PATCH /api/v1/products/:id` and update a record
7. `DELETE /api/v1/products/:id` and remove a record
8. Run `dcms codegen --lang ts` and get a `.d.ts` file with typed interfaces

No authentication required in Phase 1. No vectors. No admin UI. Just the core loop.
