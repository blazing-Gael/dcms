# DCMS

**Schema-first. AI-native. Sovereign.**

Define your data model in YAML. Get a production backend, typed SDKs, admin UI,
semantic search, and one-click deploy — without writing a line of Go.

---

## The problem

Building a backend from scratch takes weeks. Every existing CMS forces a choice:

- **Managed and locked in** — Shopify, Sanity, Contentful. You pay forever. You own nothing.
- **Flexible but you build everything** — Strapi, raw Postgres. You wire auth, migrations,
  search, admin UI, SDKs yourself.

There's no option that says: *give me a schema, give me a production-grade backend,
let me deploy it on my own infra*.

DCMS is that option.

---

## How it works

```yaml
# dcms.schema.yaml
version: "1"
collections:
  products:
    fields:
      title:       { type: string, required: true }
      price:       { type: number, required: true }
      stock:       { type: integer, default: 0 }
      status:      { type: enum, values: [draft, active, archived] }
      description: { type: text }
    timestamps: true
    indexes: [status]
    vectorize: [title, description]
```

```bash
dcms dev
# → HTTP server on :3000
# → GET/POST/PATCH/DELETE /api/v1/products
# → GET /api/v1/search?q=organic+honey&collection=products
# → GET /__schema  /__openapi  /__health
# → TypeScript types generated
# → Admin UI at /__admin
```

That's the whole setup.

---

## Features

| | DCMS | WordPress | Strapi | Payload | Sanity |
|---|---|---|---|---|---|
| Self-hosted | ✓ | ✓ | ✓ | ✓ | partial |
| Schema as code | ✓ | ✗ | partial | ✓ | ✓ |
| No vendor lock-in | ✓ | ✓ | ✓ | ✓ | ✗ |
| Built-in semantic search | ✓ | ✗ | ✗ | ✗ | ✗ |
| Single binary | ✓ | ✗ | ✗ | ✗ | ✗ |
| Plugins in any language | ✓ (Wasm) | PHP only | JS only | JS only | ✗ |
| AI scaffoldable | ✓ | ✗ | partial | partial | ✗ |
| Performance ceiling | Go | PHP | Node.js | Node.js | Cloud API |

---

## Quick start

```bash
# Install
curl -fsSL https://get.dcms.dev/dcms | sh

# Scaffold a new project
dcms new --template ecom --dir ./mystore

# Start dev server (SQLite, hot-reload)
cd mystore && dcms dev

# Generate TypeScript types
dcms codegen --lang ts --out ./types

# Migrate to Postgres
dcms migrate --db postgres://user:pass@localhost/mystore
```

---

## Client integration

### Next.js / Node.js

```typescript
import { dcms } from '@dcms/client'

const cms = dcms({ url: 'unix:///var/run/dcms.sock' })

const products = await cms.products.find({
  filter: { status: 'active' },
  sort: '-created_at',
  limit: 20,
  locale: 'ar',
})

const results = await cms.search('organic honey', { collection: 'products' })
```

### Vanilla JavaScript (no build step)

```html
<script type="module">
  import { dcms } from 'https://cdn.dcms.dev/cms.js'
  const cms = dcms({ url: 'https://mystore.com' })
  const { data } = await cms.products.find({ filter: { status: 'active' } })
  data.forEach(p => renderCard(p))
</script>
```

### Web components (zero JS)

```html
<dcms-collection name="products" filter-status="active" realtime>
  <template>
    <dcms-field bind="title" />
    <dcms-field bind="price" format="currency" />
  </template>
</dcms-collection>
```

### Raw HTTP (any language)

```bash
curl https://mystore.com/api/v1/products?filter[status]=active&limit=10
curl https://mystore.com/api/v1/search?q=honey&collection=products
curl https://mystore.com/__openapi   # full OpenAPI 3.1 spec
```

---

## Plugin system

Write plugins in any language that compiles to WebAssembly:

```rust
// Rust plugin — compiled to .wasm, dropped in /plugins
#[no_mangle]
pub fn after_create_order(record_json: *const u8, len: usize) -> i32 {
    let record = parse_record(record_json, len);
    dcms_http_post(
        "https://api.mystore.com/notify",
        &format!(r#"{{"order_id":"{}"}}"#, record["id"]),
        "{}",
    );
    0
}
```

Plugins are sandboxed by Wazero — zero filesystem or network access unless explicitly granted.
Write in Rust, Python (Extism), JavaScript, C, Zig, or any language with a Wasm target.

---

## Architecture

```
                    ┌─────────────────────────────┐
                    │         Your app             │
                    │  (Next.js / Flutter / any)   │
                    └────────────┬────────────────┘
                                 │ Unix socket / HTTP
                    ┌────────────▼────────────────┐
                    │           DCMS               │
                    │  ┌─────────────────────────┐ │
                    │  │     Schema compiler      │ │
                    │  │  YAML → routes → types   │ │
                    │  ├─────────────────────────┤ │
                    │  │      store layer         │ │
                    │  │  SQLite / Postgres / CB  │ │
                    │  ├─────────────────────────┤ │
                    │  │    Vector pipeline       │ │
                    │  │  inline · pgvector       │ │
                    │  ├─────────────────────────┤ │
                    │  │    Wazero runtime        │ │
                    │  │  sandboxed .wasm plugins │ │
                    │  └─────────────────────────┘ │
                    └─────────────────────────────┘
```

Single binary. No PHP-FPM, no Varnish, no Solr. One config file.

---

- [`CONTEXT.md`](./CONTEXT.md) — architecture overview and engine constraints
- [`ROADMAP.md`](./ROADMAP.md) — where the project is heading
- [`docs/SCHEMA_SPEC.md`](./docs/SCHEMA_SPEC.md) — the complete schema language reference
- [`docs/STORE_INTERFACE.md`](./docs/STORE_INTERFACE.md) — the storage interface contract
- [`docs/DEV_ROADMAP.md`](./docs/DEV_ROADMAP.md) — the phased build plan with acceptance criteria
- [`docs/adr/`](./docs/adr/) — architecture decision records (the "why")
- [`examples/farmly.schema.yaml`](./examples/farmly.schema.yaml) — a real e-commerce schema
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) · [`SECURITY.md`](./SECURITY.md) · [`CHANGELOG.md`](./CHANGELOG.md)

---

## Product tiers

**Open source core (this repo, MIT)**
Everything in the feature table above. Community contributions welcome.

**AI Builder** *(coming soon)*
Brief → schema → deploy in minutes. Managed hosting. Visual dashboard editor.

**Enterprise**
Multi-node cluster, Couchbase, CRDT collaborative editing, MCP server, SAML/SSO, SLA.

---

## Contributing

Issues, PRs, and plugin submissions welcome — see [`CONTRIBUTING.md`](./CONTRIBUTING.md).
Read [`CONTEXT.md`](./CONTEXT.md) before opening a PR — especially the constraints section.
The `store` interface is locked; proposals to change it need a discussion issue first.
By participating you agree to the [Code of Conduct](./CODE_OF_CONDUCT.md).

---

## License

MIT — see [LICENSE](./LICENSE)
