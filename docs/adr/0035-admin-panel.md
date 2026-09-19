# ADR-0035 — Admin panel: an embedded, server-rendered window into the instance

Status: Accepted
Date: 2026-09-19

## Context

DCMS is API-only. A headless API is unusable by the people a CMS is *for* —
content editors, and non-technical operators who need to see what the system holds.
Without a UI the instance is a black box: you can't browse records, inspect the
change feed, see who's logged in, or understand the data model without reading YAML
and curling endpoints. That's the gap between "a headless API exists" and "a CMS a
real team can run a site on," and it's a bigger unblock than any further plugin work
(ADR-0031/0032), which serves developers, not editors.

Everything needed to *generate* such a UI already exists: the schema is the single
source of truth (ADR-0001) and carries field types, labels, hints, enum values,
relations, lifecycle, `object_list` shapes, access rules, and the #27 transition
tables; the engine already tracks a rich system state (`_events`, `_revisions`,
`_media`, `_users`, `_sessions`, `_notifications`, webhooks, scheduled publishes).
The admin panel is the consumer that cashes all of that in.

## Decision

Ship an **embedded, server-rendered admin panel at `/__admin`**, generated from the
schema, that is first a **window into the whole instance** and second a content
editor.

### Stack: Go templates, embedded, progressively enhanced

- **`html/template` + `//go:embed`**, served from the binary. No SPA, no Node/Vite,
  no separate build step — the admin stays single-binary and compile-and-ship
  (ADR-0033). The templates render straight from the parsed `SchemaDefinition`
  in memory; no `/__schema` round-trip.
- **One hand-written CSS design system** (tokens, light/dark, accessible) embedded
  alongside — no CSS framework, no build.
- **Progressive enhancement:** every form works with a plain POST and no JS.
  **htmx** (small, no build) drives the dynamic bits — inline edit, live
  filtering, partial refresh — and richer widgets (RTE, drag-drop ordering) are
  vanilla JS added on top. If JS fails, the admin still works. Robustness first.

### Observability first — the surface

The panel is organized as an inspection tool:

- **System overview** — collections with row counts, recent activity, health.
- **Collection browser** — list / filter / paginate / create / edit / delete,
  with widgets derived from the schema (types → inputs, enums → selects, relations →
  pickers, lifecycle → status actions, #27 transitions → selects showing only the
  legal next states, media → upload).
- **System views** — the change feed (`_events`), per-record revision history, the
  media library, users + live sessions, webhook deliveries + dead-letters, scheduled
  publishes, the notification outbox. These are the engine-managed collections the
  API hides; the admin reads them directly (admin-role-gated) so an operator can see
  the whole machine.
- **Data-model visualization** — a **read-only** view of collections, fields, and
  the relation graph, so a non-technical person understands the structure.

### CRUD reuses the authorized write pipeline

The admin is a **privileged client, not a backdoor.** Every write goes through the
same path as the API — validation, access rules, field rules, #27 transitions,
hooks, revisions, events — with the logged-in principal. It never reaches past the
gateway to the store. Therefore it operates **as your role** (an editor sees and
does what an editor may; there is no god-mode bypass), and system views are gated by
admin role.

### Schema is read-only in the admin

The admin **visualizes** the schema but does not edit it. Live schema editing would
make the running schema diverge from `schema.yaml` — the config drift the whole
declarative-file + migrate + compile-and-ship model (ADR-0001/0033) exists to
prevent — and would require a live-migration engine with no review or rollback.
Schema changes stay a deploy step: edit the file, migrate, redeploy. A later,
optional **"design & export YAML"** tool may generate schema snippets for a human to
review and commit, but it never applies changes live.

### Security

- **Reuse session auth** (ADR-0016/0029): a server-rendered login sets the session
  cookie; the panel requires authentication, and system views require admin role.
- **CSRF tokens on every mutating form.** A cookie-authenticated, form-based UI needs
  them (the JSON API's bearer path does not); this is a first-class requirement, not
  an afterthought.
- Introspection of the data model here is gated by login, independent of the public
  `/__schema` introspection flag.

### Config-driven and extensible

- An **`admin:`** config block controls the panel: enable/disable, per-collection
  visibility and order, per-field visibility, read-only mode, which system views
  appear, and branding. Built on the existing config layering.
- **Custom admin pages** are a later payoff of the extension model (ADR-0031): a
  compile-and-ship operator registers a custom page/widget through the `App` builder,
  the same way as a custom route, rendered into the admin shell — no new machinery.

## Phasing

1. **Vertical slice** — login → system overview → one collection's list + generated
   create/edit/delete for scalar/enum fields, with CSRF. Proves embed +
   schema-driven rendering + pipeline reuse end to end.
2. Relations (pickers + `?expand`), media library + upload, lifecycle actions,
   `object_list`, #27 transition-aware selects, revision history, and the system
   views (events, users/sessions, webhooks, scheduled, notifications).
3. Dashboards (the `aggregate` + SSE surface), RTE, drag-drop ordering, the `admin:`
   config customization, and custom admin pages.

## Consequences

- A new `internal/admin` package (handlers + embedded templates/CSS/JS), mounted at
  `/__admin`, is the home; it reuses the gateway's authorized read/write paths.
- The panel is the first real consumer of the whole schema contract, so building it
  validates and hardens `/__schema`-level completeness.
- The single-binary, no-build, progressively-enhanced choice keeps the admin aligned
  with compile-and-ship and maximizes robustness, at the cost of the richer
  interactions an SPA would make easier — which htmx + targeted vanilla JS recover
  where they matter.
- Schema stays a deployed artifact; the admin never becomes a second source of truth.
