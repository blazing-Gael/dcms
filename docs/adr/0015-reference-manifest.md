# ADR-0015 — Reference resolution via a deduplicated manifest

**Status**: Accepted

## Context

Rich content documents reference other entities — an image block points at a
`_media` asset, a `reference` node/annotation points at a record (ADR-0014).
`?expand=<richtext field>` resolves those references for the client.

The first implementation inlined each resolved record into the document, mutating
the reference node with a `_resolved` object. That shape has three problems, all of
which get worse at scale:

1. **Cache invalidation.** If the resolved entity is embedded in every document
   that references it, changing one image's `alt` staleness-poisons every article
   that embeds it — you must invalidate hundreds of cached responses because one
   asset changed.
2. **Duplication.** An image referenced by 50 articles in a list response is
   serialized 50 times.
3. **Cycles.** A → B and B → A (or A → A) make inline resolution unsafe the moment
   resolution is allowed to recurse.

The established answer (Sanity's reference resolution, JSON:API's `included`,
GraphQL normalized caches) is to keep the document **pure** and return referenced
entities **once**, in a flat map, alongside the data.

## Decision

`?expand=<richtext field>` leaves the richtext AST **id-only** and returns the
resolved entities in a **deduplicated manifest at the response root**, keyed
`"<collection>:<id>"`:

```jsonc
{
  "data": { "id": "art_1", "body": [
    { "_type": "image", "ref": "med_1" },
    { "_type": "reference", "collection": "authors", "ref": "a_9" }
  ]},
  "included": {
    "_media:med_1": { "id": "med_1", "url": "…", "alt": "…" },
    "authors:a_9":  { "id": "a_9", "name": "Ada" }
  }
}
```

Consequences of the flat map:

- **Cacheable.** The document body is stable (changes only when the document
  changes); each referenced entity is cached/invalidated on its own key.
- **Deduplicated.** On a list, one shared `included` map holds each entity once,
  no matter how many documents reference it.
- **Cycle-proof.** The manifest is a set keyed by id, so A, B, and self-references
  each appear once and there is nothing to recurse into infinitely.

Resolution reuses the relation-expansion path: references are gathered across every
document in the page, then fetched with **one `id IN (…)` query per target
collection** (no N+1) and filtered by the request's **lifecycle view** (a hidden or
missing target is simply absent from `included`, ADR-0012).

### Two expansion mechanisms, by field type

`expand` on a **relation** field keeps the inline "id-or-object" union (Stripe
style) already shipped in the SDK types — it is bounded, typed, and clients depend
on it. `expand` on a **richtext** field uses the manifest, because the AST must
stay pure and content references are the dedup-/cycle-/cache-sensitive case.
Unifying relations into the manifest is possible later but is deliberately out of
scope so we don't break the relation contract before auth.

### Media is returned as a public projection

Wherever a media asset is serialized to a client — the manifest, belongs-to/file
expansion, and the `/__media` endpoints — it carries its derived `url` and display
metadata but **never `storage_key`** (the internal blob path). Resolving a
reference must not leak storage internals.

## Consequences

- Clients stitch `ref` → `included["<collection>:<id>"]` (a one-line lookup); the
  SDK ships a helper.
- The manifest is additive to the existing envelope (`data` + `meta`); responses
  without expansion are unchanged.
- A dangling or hidden content reference is expressed by absence from `included`,
  never an error.

## Deferred

- Unifying **relation** expansion into the manifest (one model for all references).
- Selecting the **projection** of included entities (sparse fields on `included`).
