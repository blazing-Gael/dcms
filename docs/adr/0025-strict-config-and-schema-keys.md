# ADR-0025 — Strict config and schema keys: an unknown key is an error

Status: Accepted
Date: 2026-09-16

## Context

Config and schema were decoded leniently. `internal/config` used `yaml.Unmarshal`
with no `KnownFields`, so an unknown key was dropped. Collection directives fell to
a `default:` branch in `internal/schema/parser.go` that skipped anything not yet
parsed — indistinguishable from a typo.

The cost was real and silent (GitHub #35, found in Agrojatra): the config set
`server.rate_limit.requests_per_minute: 60` and `burst: 20` — neither key exists —
so a public form ran on the 6000/min default with nothing logged. Validation
passed; startup said nothing. A one-letter slip does the same: `publising: true`
leaves a collection with no lifecycle (records public the instant they're created),
`soft_delet: true` makes deletes permanent, `revision: true` keeps no history.

Access actions and field-access keys already rejected unknown keys
(`parseAccess`/`parseFieldAccess`); this extends that stance everywhere.

## Decision

**Unknown keys are errors, at load/compile time, everywhere they were silent.**

- **Config** (`config.Load`) decodes with `yaml.Decoder.KnownFields(true)`. An
  unknown key at any depth aborts with the yaml path (line + field + type). Keys
  bound to env-only secret fields (`yaml:"-"`) are unknown here too, so a secret
  left in the file is rejected rather than ignored (reinforces ADR-0009).
- **Schema** top-level and `meta` keys decode with `KnownFields(true)`.
- **Collection directives**: the `default:` branch now rejects an unrecognised
  directive with a Levenshtein **did-you-mean**, except for a small set reserved
  for later phases — `vectorize`, `i18n`, `hooks`, `schedule` — which compile but
  produce a **warning** ("recognized but not implemented yet"), surfaced at server
  startup and by `dcms validate`.
- `dcms validate` loads the config (so it catches config-key errors) and prints
  schema warnings.

### Why reserved-directives warn instead of error

These directives are documented in SCHEMA_SPEC as forthcoming. Erroring would stop
a user from forward-declaring intent (`vectorize: [title]`) that will work in a
later release; silently accepting would let them believe it already works. A
warning is the honest middle: the schema compiles, and startup says plainly that
the directive does nothing yet. When a phase lands, its directive graduates from
the reserved list to a real `case`, and the warning disappears on its own.

### Why a hard break, not a warn-then-error rollout

The issue floated warning for a release before erroring, to spare deployments with
stray keys. Rejected: the whole failure mode is *booting on an unintended default*
(6000/min on a public form). A warn phase would keep booting insecurely for another
release — the opposite of the point. DCMS is 0.x (breaking changes are in-band per
the changelog policy), and `dcms validate` gives operators a pre-flight to find the
offending key before deploy. So the key error is immediate. This is noted as
BREAKING in the changelog with an upgrade step.

### Scope

Field *property* keys in a field's full form (`type`/`required`/`min`/…) are still
decoded leniently by yaml's struct decoder — `yaml.Node.Decode` exposes no
`KnownFields`, and tightening it means a parallel key check that can drift from the
struct. Deferred; the driving bug was directives and config, both now strict.

## Consequences

- A typo that used to disable a setting silently now fails loudly at load time,
  with a did-you-mean for directives and a yaml path for config.
- `dcms validate` is a real pre-deploy gate for both schema and config.
- BREAKING: a deployment whose config or schema carries a stray/misspelled key stops
  starting until the key is fixed (or removed). The example `farmly.schema.yaml`
  dropped a non-schema `brand:` block it had been silently ignoring.
- Reserved future directives stay declarable without pretending to work.
- Builds on ADR-0001 (schema is the single source of truth) and ADR-0009 (layered
  config); generalises the unknown-key rejection already used by `access:`.
