# ADR-0036 — Cross-collection membership rules (`exists`), boolean rule composition (`all`/`any`), and the store `Subquery` + boolean-filter primitives

Status: Accepted
Date: 2026-10-21

## Context

An access rule (ADR-0016) can match the caller against the **record** — `owner`
(created_by), `owner_field` (a named relation). It cannot match the caller against
a **row in another collection**. There is no way to say "readable if the caller has
an active enrollment for this record's course" (GitHub #64).

That gap is edtech's immediately (enrollment-gated lessons, guardian-scoped grades,
batch-teacher-scoped submissions), but it is not edtech-shaped. Membership authz is
everywhere DCMS is meant to serve: a doc readable by its owner *or* someone it is
shared with; a record readable by its owner *or* a member of its org/team; a story
editable by its author *or* an assigned editor. Today every one of these becomes a
custom route that re-implements the check — authz outside the rule engine, invisible
to `/__schema`, OpenAPI, and the admin panel, and a fresh chance for a hole.

Two further limits block the same class of rule:

- The only composite is `any:` (OR), and `validateOwnerFields` permits **at most one
  owner scope** in a tree — so "owner OR shared", "author OR editor" (two
  `owner_field`s), and "has the teacher role AND is assigned to this batch" cannot be
  written at all.
- The `store` interface is **locked** (ADR-0003; reaffirmed by ADR-0021). A list read
  gated by membership wants a semi-join (`course IN (SELECT …)`), which the current
  flat, AND-only `[]Filter` cannot express, and which a naive design would solve by
  adding a method to the locked interface.

## Decision

Three coordinated additions — one schema feature, one rule-composition feature, and
the store primitive that makes both expressible as a single indexed query.

### 1. The `exists` rule (schema)

A rule kind that is satisfied when a row exists in another collection linking the
record to the caller. Composable anywhere a rule is (`read`/`create`/`update`/
`delete`/`preview`/`publish`, field `read`/`write`, transition `who`).

```yaml
lessons:
  access:
    read:
      any:
        - admin
        - exists:
            in: enrollments            # the membership collection
            match: { course: course }  # enrollments.course == lessons.course
            principal: user            # enrollments.user == caller
            where: { status: active }  # optional equality filters (literals)
```

- `in` is a declared collection. `match` is `{ <field-on-in>: <field-on-this> }`,
  both belongs-to **relations** (real columns — same rule as `owner_field`).
- `principal` is a belongs-to relation on `in` to `_users`; it is bound to the caller.
- `where` is optional literal equality filters on `in`.
- **Single hop.** The record's referenced entity must join directly to a membership
  table keyed by the caller. A rule needing an intermediate entity (enrolled in the
  *program* that contains the course) is a **second hop**, reserved as an additive
  `through:` extension and not built here — every concrete need today is single-hop.
- **Uncorrelated by construction.** The membership lookup is parameterized only by the
  caller and `where` literals, never by the outer record. This is both why it is cheap
  (one indexed lookup, reusable across the whole list) and why it is portable (see §3).
- The rule **auto-declares an index** on `in`'s `principal` column (ideally
  `(principal, match-field)`), so a gated list never full-scans the membership table.

Validation: `in` declared; `match` keys/values are non-`many` relations on each side;
`principal` is a non-`many` relation to `_users`; `where` fields exist and type-check.
The membership collection's *own* read rule is **not** re-applied — `principal` already
binds every matched row to the caller, so the existence of the row is the grant.

### 2. `all:` and `any:` composition

The rule language becomes a complete, nestable boolean algebra. `any:` (OR) already
exists; add `all:` (AND). A sub-rule may be any rule, including a nested composite or
an `exists`.

```yaml
# coarse role gate AND fine membership gate — the common teaching-assistant shape
submissions:
  access:
    read:
      all:
        - teacher
        - exists: { in: batch_teachers, match: { batch: batch }, principal: user }
```

`all:` is *easier* than `any:` to enforce: the top-level filter list is already ANDed,
so `all:[role, {exists}]` is "require the role (collection-scope) AND append the exists
filter" with no new node. Only AND-inside-OR needs the recursive `And` node from §3.
Degenerate combinations are rejected or simplified at compile (`any:[public, …]` ≡
public is flagged, not silently opened).

### 3. Store primitives (`Subquery`, `And`/`Or`) — amending the ADR-0003 lock

ADR-0003 locks the `store` interface to keep every adapter (SQLite now; Postgres soon;
Couchbase later) compiling against one contract. That lock is about **methods** — the
thing that breaks adapters at compile time. These additions add **no method and change
no signature**; they widen what an `In`/`NotIn` filter's value may be and add two
boolean operators, reusing the existing `Value any`:

```go
// A subquery as an In/NotIn value. Filters are literal-only ⇒ uncorrelated.
type Subquery struct {
    Collection string   // "enrollments"
    Select     string   // "course" — the match column projected out
    Filters    []Filter // {user = caller}, {status = "active"} — literals only
}

const (
    And Op = "and" // Value is []Filter — all must hold
    Or  Op = "or"  // Value is []Filter — any must hold
)
```

`buildWhere` becomes recursive: a leaf → `col OP ?`; an `Or`/`And` node →
`(c1 OR/AND c2 …)`; a `Subquery` value under `In` → `col IN (SELECT select FROM coll
WHERE …)`. The top-level `[]Filter` still means AND, so **every existing call site is
untouched.** `Query.Filters` and `Filter` keep their shapes.

This is a deliberate, bounded relaxation of ADR-0003's strict "no new operators"
reading. It is safe to make now — there is a single adapter and a single consumer
(the author's own products), so the contract can still evolve additively; the lock
takes full effect before the store goes public, with these primitives in it. It is
recorded as an **additive store version bump** and guarded two ways so the widening
can never silently degrade:

- An adapter that has not implemented a `Subquery`/`And`/`Or` returns `ErrInvalidInput`
  (a clean, detectable failure — never a wrong answer).
- A **cross-adapter conformance suite** exercises the semi-join and boolean nodes; any
  future Postgres/Couchbase adapter must pass it to be considered complete.

### Evaluation model

`evalRule` is generalized from returning `(decision, field)` — one owner scope — to
returning scope **branches**, each a filter producer (`owner`/`owner_field` → an `Eq`
filter; `exists` → an `In`-subquery filter). The combinators assemble them:

- `any:` → `allow` short-circuits (an admin never needs a filter); otherwise the
  surviving branches combine into one `Or` node.
- `all:` → `allow` iff every branch allows; the branch filters are ANDed; any `deny`
  denies.

`listReadFilters`, `recordReadable`, the write-auth paths, and `mediaReferencedReadable`
(inherit-media, ADR-0028) all consume "a `[]Filter` to AND onto the query" that may now
contain `Or`/subquery nodes. The old "one owner scope" restriction is **removed** —
replaced by "every branch must be expressible", which the new nodes satisfy.

## Consequences

- Membership, owner-or-shared, author-or-editor, and role-and-membership become
  declarative rules, visible in `/__schema`/OpenAPI/admin for free (JSON tags) — not
  custom routes. One tested engine path replaces every app's hand-rolled check.
- `_media.access.read: inherit` follows membership automatically (#64 open question):
  a file is readable iff the caller can read a referencing record, and "can read" now
  includes membership, because inherit reuses the referencing collection's read filters.
- **One indexed query per list**, no materialized id list, no N+1. The membership
  subquery is uncorrelated, so SQLite/Postgres run it as a hash semi-join and Couchbase
  N1QL supports (and hash-optimizes) the same `field IN (SELECT …)` form within a scope
  — no `USE KEYS`, no cross-bucket join. Portability is a property of the design, not a
  hope.
- **Bounded on purpose.** Single hop; uncorrelated; equality `where` only. A second hop
  and non-equality predicates remain additive future work or a hook's job (ADR-0031).
- The store gains surface (one value type, two ops, recursive `buildWhere`) across every
  adapter, guarded by the conformance suite. No method, no signature change, no existing
  call site touched; `STORE_INTERFACE.md` and the version note record the widening.
- The authz return model changes shape once (here), rather than twice (membership now,
  OR later) — the reason this ADR bundles all three.

## Alternatives considered

- **Two-step id fetch in Go** (query memberships → feed as an `In` literal list). Zero
  store change, and the fallback if §3 is rejected. Rejected as the primary design: two
  round trips, an unbounded `IN (…)` list on large memberships, and it does not
  generalize to OR or to other cross-collection filters.
- **A new `DB` method (`SemiJoin`/`Exists`).** Breaks the ADR-0003 lock the hard way
  (every adapter stops compiling) to buy nothing the `Subquery` value doesn't.
- **Correlated subquery / per-row check.** The Couchbase-hostile form (needs `USE KEYS`
  or a JOIN rewrite). Avoided by keeping the lookup uncorrelated.
- **`RawQuery`.** Forbidden by the store contract for user-facing reads, and
  SQL-dialect-specific — the opposite of portable.
- **Replacing `Query.Filters` with a condition tree.** The clean data model, but it
  breaks every call site that builds `[]Filter`. Additive `And`/`Or` nodes get the same
  expressiveness with no migration.
