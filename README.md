# LxCMS

**Schema-first. AI-native. Sovereign.**

Define your data model in YAML. Get a production backend, typed SDKs, admin UI,
semantic search, and one-click deploy — without writing a line of Go.

Part of the [LxRoot](https://lxroot.io) ecosystem.

---

## The problem

Building a backend from scratch takes weeks. Every existing CMS forces a choice:

- **Managed and locked in** — Shopify, Sanity, Contentful. You pay forever. You own nothing.
- **Flexible but you build everything** — Strapi, raw Postgres. You wire auth, migrations,
  search, admin UI, SDKs yourself.

There's no option that says: *give me a schema, give me a production-grade backend,
let me deploy it on my own infra*.

LxCMS is that option.

---

## How it works

```yaml
# lxcms.schema.yaml
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
lxcms dev
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

| | LxCMS | WordPress | Strapi | Payload | Sanity |
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
brew install lxroot/tap/lxcms   # macOS
# or: curl -fsSL https://get.lxroot.io/lxcms | sh

# Scaffold a new project
lxcms new --template ecom --dir ./mystore

# Start dev server (SQLite, hot-reload)
cd mystore && lxcms dev

# Generate TypeScript types
lxcms codegen --lang ts --out ./types

# Migrate to Postgres
lxcms migrate --db postgres://user:pass@localhost/mystore
```

---

## Client integration

### Next.js / Node.js

```typescript
import { lxcms } from '@lxroot/cms-client'

const cms = lxcms({ url: 'unix:///var/run/lxcms.sock' })

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
  import { lxcms } from 'https://cdn.lxroot.io/cms.js'
  const cms = lxcms({ url: 'https://mystore.com' })
  const { data } = await cms.products.find({ filter: { status: 'active' } })
  data.forEach(p => renderCard(p))
</script>
```

### Web components (zero JS)

```html
<lx-collection name="products" filter-status="active" realtime>
  <template>
    <lx-field bind="title" />
    <lx-field bind="price" format="currency" />
  </template>
</lx-collection>
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
    lxcms_http_post(
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
                    │           LxCMS              │
                    │  ┌─────────────────────────┐ │
                    │  │     Schema compiler      │ │
                    │  │  YAML → routes → types   │ │
                    │  ├─────────────────────────┤ │
                    │  │       mql ORM            │ │
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

## Project structure

See [`CONTEXT.md`](./CONTEXT.md) for the full architecture and development guide.
See [`SCHEMA_SPEC.md`](./SCHEMA_SPEC.md) for the complete schema language reference.
See [`MQL_INTERFACE.md`](./MQL_INTERFACE.md) for the storage interface contract.
See [`DEV_ROADMAP.md`](./DEV_ROADMAP.md) for the phased build plan.
See [`examples/farmly.schema.yaml`](./examples/farmly.schema.yaml) for a real e-commerce schema.

---

## Product tiers

**Open source core (this repo, MIT)**
Everything in the feature table above. Community contributions welcome.

**[AI Builder](https://lxroot.io/builder)** *(coming soon)*
Brief → schema → deploy in minutes. Managed on LxRoot. Visual dashboard editor.

**[Enterprise](https://lxroot.io/enterprise)**
Multi-node cluster, Couchbase, CRDT collaborative editing, MCP server, SAML/SSO, SLA.

---

## Contributing

Issues, PRs, and plugin submissions welcome.
Read [`CONTEXT.md`](./CONTEXT.md) before opening a PR — especially the constraints section.
The `mql` interface is locked; proposals to change it need a discussion issue first.

Plugin authors: publish to the registry at [plugins.lxroot.io](https://plugins.lxroot.io).

---

## License

MIT — see [LICENSE](./LICENSE)

Built by [LxRoot](https://lxroot.io) · Made in Bangladesh 🇧🇩
