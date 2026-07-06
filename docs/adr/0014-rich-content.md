# ADR-0014 — Rich content fields (structured `richtext`)

**Status**: Accepted

## Context

A CMS is defined by its body content. A plain `text` field can't express
formatted prose (headings, bold, links, lists) or — the part that makes this a
*content engine* rather than a textarea — **embeds that reference other records
and media assets** inline in the flow of the text. We need a first-class rich
content field.

The foundational choice is the storage format. The options and why we rejected
two of them:

- **HTML string** — maximum renderer lock-in (web only), a sanitization/XSS
  burden on every read, and brittle inline embeds. Rejected.
- **Markdown string** — simple, but lossy for embeds and annotations, no
  first-class references, weakly typed, and you re-parse it to extract text or
  references. Rejected as the canonical form.
- **Structured JSON document** — a portable-text-style node tree. Renderer-
  agnostic (one value → HTML / React / plain text), references are first-class
  nodes, safe by an allowlist of node/mark types, and it is queryable and
  walkable for search and embeddings. This is the modern headless-CMS consensus
  (Sanity Portable Text, Contentful Rich Text). **Chosen.**

Constraints carried forward:

- The `store` interface is **locked** (ADR-0003): a rich field must ride an
  existing column type, not a new one.
- **Performance is non-negotiable**: validating a body must add no N+1 — any
  in-content references are checked in the same batched way as relations
  (ADR-0010).
- Everything **configurable** (ADR-0002): what a given field may contain
  (styles, marks, block types) is tunable per field, with sensible defaults.

## Decision

Add a schema field type `richtext`. Its value is a **structured JSON document**,
stored in a `json` column — so the locked store is untouched (`richtext` is a
distinct *schema* type only for validation, codegen, and reference handling).

### The document model (portable-text-inspired)

A value is a JSON **array of nodes**, each discriminated by `_type`:

- **`block`** — a text block (the core prose node, always allowed):
  ```
  { _type:"block", style:"h2", listItem?:"bullet"|"number", level?:int,
    markDefs:[ {_key, _type:"link", href}
             | {_key, _type:"reference", collection, ref} ],
    children:[ span … ] }
  ```
- **`span`** — inline text inside a block:
  ```
  { _type:"span", text:"…", marks:[ "<decorator>" | "<markDef _key>" ] }
  ```
  A mark is either a **decorator** (`strong`, `em`, `code`, `underline`,
  `strike`) or the `_key` of an annotation in the block's `markDefs`.
- **Custom blocks** (opt-in per field) — non-text nodes:
  - `{ _type:"image", ref:"<_media id>", alt?, caption? }` → references `_media`
  - `{ _type:"reference", collection:"<name>", ref:"<id>" }` → references a record
  - `{ _type:"code", code:"…", language? }`
  - `{ _type:"embed", url:"…" }`

### Per-field configuration

```yaml
body:
  type: richtext
  styles: [normal, h2, h3, blockquote]   # allowed block styles   (labels; free-form)
  marks:  [strong, em, code, link]       # allowed decorators + annotation types
  blocks: [image, reference]             # allowed custom (non-text) block types
```

Omitted lists fall back to defaults: `styles = [normal, h1, h2, h3, h4,
blockquote]`, `marks = [strong, em, code, link]`, `blocks = [image]`. `marks`
entries must be known decorators/annotations and `blocks` entries must be known
block types (`image`, `code`, `embed`, `reference`) — an unknown one is a schema
validation error. `styles` are free-form labels (a renderer maps them).

### Validation is split by layer (mirrors relations, ADR-0010)

- **Structural** (schema, pure, no DB): the value is a well-formed document —
  array of nodes, each a known/allowed `_type`, blocks carry a valid `style` and
  spans whose marks resolve to a decorator or a declared `markDef`, custom blocks
  have their required fields, link `href`s use a **safe scheme** (`http`, `https`,
  `mailto`, or relative — never `javascript:`/`data:`), and the node count is
  under a bound. A failure is a field-named `422`.
- **Referential** (gateway, batched): every in-content reference (image → `_media`,
  `reference` node/annotation → its `collection`) is harvested and checked for
  existence **in the same batched pass as belongs-to and m2m ids** — one
  `id IN (…)` query per target collection, no per-node query. A miss (or an
  unknown/unroutable reference collection) is a field-named `422`.

Because in-content references live inside a JSON blob, there is **no database
foreign key** protecting them — the validation layer owns their correctness,
exactly the graceful-degradation posture ADR-0010 already describes for adapters
without FK support.

### Read-side expansion

> **Note:** the `_resolved`-on-the-node mechanism described here was superseded by
> **ADR-0015** before release — resolved entities now go into a deduplicated root
> `included` manifest and the AST stays id-only. The batching and lifecycle
> semantics below are unchanged.

`?expand=<richtext field>` inlines the referenced records into the document:
image blocks resolve their `_media` asset (so a renderer gets the `url`) and
`reference` nodes/annotations resolve the named record. The target is attached
under an additive `_resolved` key on the node/markDef (the `ref` id is kept), so
the shape only grows. Resolution reuses the relation-expansion machinery: it is
**batched** (references gathered across every document in the page, then one
`id IN (…)` query per target collection — no N+1) and **lifecycle-filtered** (a
reference to a hidden or missing record is left unresolved, mirroring how a hidden
belongs-to target keeps its id, ADR-0012). This also settles the read behavior for
a dangling in-content reference: it simply doesn't resolve, never a 500.

### Codegen & contract

One shared `RichText` type is emitted in each SDK (a `RichTextBlock[]`), and the
OpenAPI/JSON-schema contract gains a reusable `RichText` component that every
`richtext` field `$ref`s — so the type is defined once and stays in lockstep with
the validator (ADR-0008).

## Consequences

- Rich bodies reuse the existing media, relations, and referential-integrity
  machinery instead of a parallel stack.
- Per-write cost on `richtext` fields is a bounded tree-walk plus the existing
  batched existence query — no read-path cost, no N+1.
- We store structure, not markup: rendering is the client's job (the SDK ships
  the type; a serializer can follow), and there is no stored-HTML XSS surface.

## Deferred

- **Server-side plain-text extraction** feeding full-text search and the
  embedding pipeline (the tree is walkable for exactly this).
- **A write-time delete policy for in-content references** (blocking or cleaning
  up when a referenced asset/record is removed later): since no FK protects them,
  a stale ref is currently harmless on read (it simply doesn't resolve — see
  Read-side expansion), but there is no `on_delete`-style guard at delete time yet.
  A configurable policy (or a background integrity sweep) is a later refinement.
- **Modular "blocks" / page-builder** content (an ordered array of heterogeneous
  section components) as its own field type, distinct from inline prose.
- **Arbitrary user-defined custom block types** with declared shapes.
