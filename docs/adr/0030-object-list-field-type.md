# ADR-0030 — `object_list` field type: repeatable groups of fields

Status: Accepted
Date: 2026-09-17

## Context

There is no field type for a **repeatable group of fields** — a list whose
elements are small objects with a declared shape (GitHub #6). The motivating case
is art-directed marketing pages modelled as content: a `pages` document has a few
named slots (`capabilities`, a FAQ, CTA buttons) that are genuinely short lists of
small records, and a non-technical editor should edit their copy and swap photos
without touching the site repo. The important property is that the **composition
stays fixed** (the page's design is not editable) while the **content of each
element is** — so a generic block builder is the wrong tool.

Today the options are both poor:

- `type: json` holds it, but the shape is invisible to validation, OpenAPI/SDK
  codegen, and the admin UI; a malformed element is accepted silently until the
  site build trips over it.
- A child collection + a relation + an `order` integer works, but costs an extra
  collection per group, an extra round trip or `expand`, manual order maintenance,
  and a much worse editing experience for three cards that belong to one document.

## Decision

Add an `object_list` field type whose element shape is declared inline with `of`,
and implement it by **reusing `FieldDef` recursively, one level deep** — the
element's fields *are* `FieldDef`s, so every stage the engine already has for
top-level fields applies to them unchanged.

```yaml
capabilities:
  type: object_list
  max: 4
  of:
    title: { type: string, required: true }
    body:  { type: text }
    image: { type: file }
```

**Storage: JSON in a column (ADR-0003 intact).** An `object_list` translates to a
JSON column exactly like `richtext`. The SQLite adapter already marshals a Go
`[]any` into JSON text on write and the gateway decodes it on read, so there is
**no store change, no migration machinery, no join table, no `order` column**, and
no gateway write-path change. The whole list lives in the parent row and returns in
one read.

**Validation composes.** A value is an array within `min`/`max` whose every element
is validated against `of` as a complete record — `validateRecord` recursing with
`CollectionDef{Fields: f.Of}`, so inner `required`/`min`/`pattern`/`enum` come for
free. A `422` names the offending item.

**Referential integrity composes.** Inner `file`/belongs-to ids are soft references
inside the JSON blob (no FK), checked for existence on the same batched
`checkReferences` pass as top-level and `richtext` references (ADR-0014/0015). No
N+1; a dangling id is a field-named `422`.

**Contracts compose.** `fieldJSONSchema` and the TypeScript type mapper recurse
over `of` → OpenAPI `type: array` with an object `items` schema, and TS
`Array<{ _key?: string; … }>`.

**Bounds, enforced at compile time.** One level deep: an element may not contain an
`object_list`, a `richtext`, or a many-to-many relation; `of` must be non-empty.
Arbitrary recursion would be a much bigger change for little gain. Each element may
carry an optional `_key` string — the same stable-identity role `richtext`
`markDefs` play — for admin-UI reordering; it is client-supplied and reserved as an
inner field name.

## Consequences

- The feature lives almost entirely in package `schema` (parse, validate,
  translate, response-coercion, JSON-schema) plus the neutral codegen model + TS
  backend, and one branch in `gateway/refcheck.go`. Each touch point is a recursion
  into machinery that was already load-bearing, which is what keeps it small and
  maintainable.
- **`file` sugar extends inward.** `injectMedia` now rewrites an inner `file` field
  to a `_media` relation just as it does a top-level one, so an element can hold an
  image.
- **Deferred.** No per-element access rules, no `unique`-across-elements, and no
  arbitrary nesting. A genuinely editor-rearranged page, or items shared across
  documents, still belongs in a child collection with a relation — `object_list` is
  for fixed-composition, document-owned groups.
