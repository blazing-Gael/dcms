
# ADR-0017 — Exact decimal type for money

Status: Accepted
Date: 2026-08-03

## Context

DCMS's numeric field types are `number` (IEEE-754 `float64`) and `integer`.
Neither can represent money exactly: `0.1 + 0.2 != 0.3` in binary floating point,
and JSON numbers are parsed as doubles by essentially every client (JavaScript
has only `number`). A storefront that stores a price, a tax, or an order total in
a `float64` column will accumulate rounding error — the precise class of bug a
system whose pitch is *reliability* cannot ship. Before building carts, orders,
or totals (the ecom track), money needs an exact type.

This is the first item of the "M-A · correctness & hardening" milestone.

## Decision

### 1. A new scalar field type: `decimal`

```yaml
price:  { type: decimal, scale: 2 }     # money — two fractional digits
weight: { type: decimal, scale: 3 }     # any fixed-point quantity
cost:   decimal                          # shorthand → scale 2
```

`scale` is the fixed number of fractional digits (0 ≤ scale ≤ 9, default **2**).
`decimal` is a general fixed-point type, not literally "money" — currency,
rounding policy, and multi-currency are application concerns DCMS does not model.
What DCMS guarantees is **exactness**: what you write is what you read, to the
declared scale, with no floating-point drift.

### 2. Wire format is a JSON **string**, never a number

A `decimal` value crosses the API as a quoted string — `"12.50"`, `"-3.00"`,
`"0.001"` — and is always emitted at the full declared scale (`1250` at scale 2
serializes as `"12.50"`). Rationale:

- A JSON number would be re-parsed as a `float64` by the client the instant it is
  decoded, reintroducing the exact error we are eliminating. A string survives
  intact.
- On input we therefore **reject** a JSON number for a decimal field with a clear
  message, forcing the exact-string convention. A value with more fractional
  digits than the scale is also rejected — money is never silently rounded.

This mirrors how established payment APIs move money (as strings or integer minor
units), and it is what the generated SDK/OpenAPI types will advertise (`string`
with a decimal `pattern` and an `x-decimal-scale` annotation).

### 3. Storage is int64 **minor units**; the store stays dumb

A decimal is persisted as an exact integer count of minor units — value ×
10^scale — so `"12.50"` at scale 2 is the integer `1250`. On SQLite the column
is `INTEGER`; a future Postgres adapter maps the canonical `decimal` token to
`NUMERIC(precision, scale)` (an additive `ColumnMeta.Scale` when that lands —
allowed under the locked-store rule, which permits additive data fields).

Integer minor units are:
- **exact** — no binary-fraction error, ever;
- **sortable and indexable** — `ORDER BY price` and `filter[price][gte]` are plain
  integer comparisons on an indexed column, no special collation;
- **exact under aggregation** — `SUM(price)` over int64 minor units is exact
  (the one aggregate that matters for money; `AVG` is inherently fractional and
  remains the client's concern).

The **store never learns about scale or decimals.** It stores and returns an
integer, exactly as it does for any `INTEGER` column. All decimal ↔ string
conversion lives in the gateway, which is the schema-aware layer — the same split
richtext uses (the store sees JSON text; the gateway owns the structure). This
keeps ADR-0003 (auth/schema-agnostic, method-locked store) intact and means a new
adapter inherits `decimal` for free.

### 4. Conversion points (all gateway-side, all existing choke points)

- **Write (string → int64):** after request validation (which guarantees the
  string parses to the scale) and before the store write. Wired at the
  post-validation point in both write paths — the plain create/update handlers and
  the transactional nested-write path (`createRecord`/`updateRecord`).
- **Read (int64 → string):** in `CollectionDef.CoerceResponse`, the single
  read-shaping choke point already used by single reads, list reads, and
  `?expand`ed relations. One hook covers every read path.
- **Filters (string → int64):** in the list-query filter coercion, using the
  field's scale, so `filter[price][gte]=10.00` compares correctly across adapters.
  A filter value that doesn't parse is a `422`, not a silently dropped filter.

### 5. Validation

`decimal` request validation (shared by request and response contract checks per
ADR-0008): the value must be a decimal string with at most `scale` fractional
digits and must fit in int64 minor units (overflow → error). Optional `min`/`max`
bounds are compared as decimals. A `default` on a decimal field is a decimal
string; it is converted to its int64 literal at migration time so the column's
SQL `DEFAULT` is an integer.

### 6. Currencies and units are companion fields, not part of the type

`decimal` is unit-agnostic: it stores an exact number, never *what the number is
in*. The three currency/unit concerns decompose cleanly onto existing primitives:

- **Per-currency precision** is the `scale` knob: `scale: 2` for USD/EUR,
  `scale: 0` for JPY/KRW, `scale: 3` for BHD/KWD, `scale: 8` for most crypto.
  A field commits to one scale, exactly as `NUMERIC(p, s)` does.
- **Which currency/unit a value is in** is a *sibling* field — an `enum` or a
  `relation` to a `currencies` collection — because the valid set is application
  policy (a JPY-only shop must not accept USD), which enum/relation already model:

  ```yaml
  fields:
    amount:   { type: decimal, scale: 2 }
    currency: { type: enum, values: [USD, EUR, JPY] }
  ```

- **Conversion, exchange rates, rounding policy, mixed-currency arithmetic** are
  business logic with no single correct answer, and stay entirely application-side.

A `{amount, currency}` composite money type was rejected: it would need its own
validation, wire shape, filter grammar, and OpenAPI representation, while two
existing primitives compose the same model with none of that surface. The one
limitation this accepts: `scale` is fixed per column, so a single column holding
values at genuinely different precisions must pick the maximum scale it will
support (a mixed-currency ledger column uses `scale: 3` and lets JPY store
`1000` = "1.000"); the paired currency field drives display.

## Consequences

- **Money is exact end-to-end**, and the exactness is a storage/serialization
  property, not something each application re-implements.
- **A behavior change for clients:** a decimal field is written and read as a
  string. This is intentional and is surfaced in OpenAPI (`type: string`,
  `x-decimal-scale`) and the TS SDK (`string`).
- **int64 range** bounds a decimal to ±9.2×10^18 minor units — ample for money at
  scale 2–4; scale is capped at 9 so the whole-number range stays large. Values
  needing 18+ fractional digits (some crypto) are out of scope for this type.
- **No new store method, no store awareness of the type** — consistent with the
  richtext precedent (ADR-0014). No enforcement/query-path cost: reads gain one
  in-memory format per decimal field; writes one parse; filters one parse.
- **Migrations are stable:** the additive migrator matches columns by name and
  never retypes, so `decimal`→`INTEGER` does not churn on boot even though
  introspection reports `INTEGER`.

## Alternatives considered

- **Store decimals as TEXT and compare as strings** — rejected: lexicographic
  order isn't numeric order (`"9.90" > "12.50"`), breaking sort and range filters
  without fragile zero-padding, and SQLite's `NUMERIC` affinity would coerce the
  text back to a `REAL`, losing precision.
- **Keep `number` and document "don't use it for money"** — rejected: a footgun
  the type system should remove, not a caveat in prose.
- **A `big.Rat`/arbitrary-precision type on the wire** — over-scoped for a CMS;
  int64 minor units cover money and fixed-point quantities with a far simpler
  contract.
- **Accept JSON numbers for convenience** — rejected: precision is already lost by
  the time the number reaches the server; accepting it would launder a bug.
