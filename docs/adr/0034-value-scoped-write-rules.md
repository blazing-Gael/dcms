# ADR-0034 — Value-scoped write rules: role-gated enum transitions

Status: Accepted
Date: 2026-09-19

## Context

A field's `write` rule (ADR-0016 M2) is one gate: a role may write any value of the
field or none. That can't express a workflow — "a writer may move `review` from
`writing` to `submitted`, but only an editor may `approve`" (GitHub #27). The two
available shapes both fail: owner-writable lets a writer approve their own story;
editor-only stops a writer from submitting.

This could be done imperatively in a `BeforeUpdate` hook (ADR-0031). It is core
instead, because it is **authorization, not business logic** (which role may set
which value — an increment to the `access:` engine, not a new subsystem), and
because a declarative transition table is **part of the contract**: it serializes
into `/__schema`, so an admin UI can offer only the legal next states, which a hook —
by the ADR-0031 "values, not shape" guardrail — cannot expose. It is also ubiquitous
and security-shaped, so one tested engine rule beats every app re-deriving the same
checks (each a chance for a self-approval hole). Hooks remain the escape hatch for
conditions a declarative table can't express.

## Decision

Add a value-scoped form of `write:` on **enum** fields — a small, declarative
transition table evaluated by the existing rule engine.

```yaml
review:
  type: enum
  values: [writing, submitted, changes_requested, approved]
  access:
    write:
      rules:
        - who: owner
          from: [writing, changes_requested]   # allowed current values
          to:   [writing, submitted]           # allowed new values
        - who: [admin, editor]                  # from/to omitted ⇒ any transition
```

`who` reuses the full rule engine (`owner`, roles, `any:`, `owner_field`). `from`/`to`
are declared enum values; an omitted set means "any". `write:` therefore has two
shapes: a plain rule (as before) or `{ rules: [...] }` — never both.

### Evaluation

For a write where the field is present, comparing the stored value (`current`) to the
incoming value:

- **No-op** (`new == current`, on update) → **allowed**, so round-tripping a record
  never trips.
- A rule **applies** when its `who` is satisfied for the caller. Among applying rules
  the transition is **permitted** when `current ∈ from` and `new ∈ to`.
- **Permitted** → the write proceeds.
- **A rule applied but none permitted this transition** → **403 naming the field** —
  a loud error, not the silent drop used for a plain unwritable field, because a
  submit that quietly didn't happen is worse than an error.
- **No rule applied to the caller** → **silent drop**, exactly like a plain write
  rule the caller fails.
- **Create** → there is no prior value, so `from` is ignored; a create is gated by
  `who` + `to` (which initial values a role may set).

## Consequences

- The rule lives in `FieldAccess.WriteTransitions`, parsed in `parseFieldAccess`,
  validated (enum field; declared roles; `from`/`to` are declared enum values), and
  enforced in the existing field-write path (`stripUnwritableFields`, which now can
  return a 403). No new store surface, no new subsystem.
- It serializes into `/__schema` for free (JSON tags), so clients and an admin UI can
  render the legal transitions per caller.
- **Bounded on purpose.** It is a finite `from → to` table gated by `who`; it does not
  express arbitrary predicates (value depends on another field, the time, external
  state). Those remain a hook's job — declarative for the common state-machine case,
  imperative for the rest (ADR-0031). This is the "declarative front-end of hooks"
  that ADR-0031 anticipated.
- The plain `write:` rule is unchanged; a field uses one shape or the other.
