# ADR-0024 — A `publish` access rule, and revision history follows `preview`

Status: Accepted
Date: 2026-09-16

## Context

Two gaps surfaced while building Agrojatra's writer/editor apps on the API, once
identity-based preview (ADR-0023) let writers and editors work without the shared
token.

1. **Publishing was authorized like any other edit (issue #23).** The lifecycle
   transitions — `publish`, `unpublish`, `archive` — went through
   `authorizeRecordWrite(..., ActionUpdate)` (`internal/gateway/transitions.go`),
   so any caller who could edit a draft could also take it live. An editorial flow
   where writers draft but only editors ship had no way to express "may edit, may
   not publish": widening `update` to writers handed them the publish button too.

2. **Revision history was token-only (issue #24).** History reads
   (`GET /{id}/revisions`, `/revisions/:version`) were gated by `previewDenied` —
   the shared preview token or nothing (this was the explicit "out of scope" note
   at the end of ADR-0023). So the same secret problem ADR-0023 removed for records
   still applied to history: a writer couldn't see their own draft's edit history,
   an editor couldn't diff a submission, without shipping `DCMS_PREVIEW_TOKEN`.

## Decision

### A sixth access rule: `publish`

Add `publish` to `AccessRules`, gating the go-live transitions independently of
`update`:

```yaml
stories:
  publishing: true
  access:
    create:  authenticated
    update:  { any: [editor, owner] }   # owners edit their own drafts
    publish: [editor]                   # only editors take a story live
```

- It gates `publish` / `unpublish` / `archive` (the `publishTransition` set). The
  `restore` transition (undo soft-delete) is **not** a publishing change — it stays
  an `update`, its visibility governed by `preview` (ADR-0023), so nothing about
  trash/restore changes here.
- Evaluated per record through the same `evalRule` machinery every rule uses, so
  `owner` / `owner_field` / `any:` work with no new code. The transition path now
  resolves a rule (`PublishRule()` when set, else the update rule) and calls the
  shared `authorizeWriteRule` — a small refactor that splits rule *selection* from
  rule *enforcement* so both paths share one authorization body.
- **Opt-in, backward compatible.** Unset ⇒ transitions fall back to the `update`
  rule (today's behaviour); every existing schema is unchanged.

Validation: `public` in a `publish` rule tree is a schema error (anyone could
publish), and `publish` requires `publishing` (there are no go-live transitions to
gate on a collection without it).

### Revision history follows `preview`

When a collection declares a `preview` rule, its revision history follows that rule
**per record** instead of the shared token: `revisionHistoryDenied` loads the
record and reuses `previewEligible` (the record-scope preview decision, ADR-0023).
So history is visible to exactly whoever may preview the record — a writer sees
their own draft's history (`ownerScope`), an editor diffs a submission (`allow`), a
stranger gets 404. With no `preview` rule, history stays token-gated exactly as
before. The read rule still applies on top (`authorizeRecordRead`), and `restore`
continues to obey the `update`/`preview` rules like any transition.

This factors the record-scope half of `recordPreviewVisible` out into
`previewEligible` — "may this identity see behind the curtain," independent of
whether the record happens to be public — which is the right question for history
(you want an editor to see a *published* record's draft-era history too).

## Consequences

- Editorial separation of duties works: `update` for writers, `publish` for
  editors, with no store or query change — publishing is one in-memory rule
  evaluation on an already-authorized transition.
- Revision history needs no shared secret in an authoring UI; it costs one extra
  record read on collections that declare `preview` (none otherwise).
- Supersedes-in-part ADR-0023's closing note: revision-history reads are no longer
  token-only when a collection opts into `preview`.
- Both rules are opt-in; unset schemas behave exactly as before.
- Out of scope: a distinct "who may schedule vs. publish-now" split, and per-field
  publish gating — neither is needed by the driving use case.
