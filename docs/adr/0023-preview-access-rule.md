# ADR-0023 — Identity-based preview: a `preview` access rule for hidden lifecycle states

Status: Accepted
Date: 2026-09-15

## Context

Whether a caller may see a non-published record (draft, scheduled, archived, or
soft-deleted) was decided **only** by the shared preview token (`visibilityFor`,
`internal/gateway/lifecycle.go`, ADR-0012). Identity and `access:` rules played
no part: without the token, every caller — the record's own author, an editor, an
admin — got the public view (published, `_published_at <= now`, not trashed).

This blocks any authoring UI built on the API (GitHub issue #20, found building
Agrojatra's writer/editor apps):

- a writer can create and edit a draft but can never list or re-open it;
- an editor can't see a review queue (submitted work is all drafts);
- the only workaround is shipping `DCMS_PREVIEW_TOKEN` to the browser — one shared
  secret that reveals **every** user's drafts, so it cannot go in a client.

Two smaller inconsistencies came with it: `handleUpdate`/`handleDelete` didn't
consult visibility at all, so a caller could write to a record the read path
reported as non-existent; and there was no way to let an owner restore their own
trashed record without the shared token.

## Decision

Add a fifth access rule, `preview`, that says **who may see and act on a
collection's hidden lifecycle states** — draft/scheduled/archived **and** trashed.

```yaml
stories:
  publishing: true
  soft_delete: true
  access:
    read:    { any: [admin, editor, builder, owner] }
    preview: { any: [admin, editor, owner] }
```

It is evaluated per record through the same `evalRule` machinery every other rule
uses, so it rides the existing dual:

- **collection scope** (lists): `allow` honours the requested `?status` /
  `?include_deleted`; `ownerScope` honours them and narrows to the caller's own
  rows (an owner filter, exactly as `read: owner` does); `deny` forces the public
  view. This composes into store filters — no post-filtering.
- **record scope** (get-one, expansion, writes): `allow`, or `ownerScope` with the
  record's owner matching the caller, makes the record visible in any hidden
  state; otherwise the public view applies (→ 404 when hidden).

`owner` and `owner_field` work in `preview` for free.

### Opt-in, and the single source of truth when opted in

`preview` is **opt-in per collection**, like `publishing`/`revisions`/`events`:

- **Unset** ⇒ today's behaviour exactly: hidden states are visible only with the
  shared token on reads, and writes are unguarded by visibility. Every existing
  schema is unchanged.
- **Set** ⇒ the rule is the single source of truth for hidden-state visibility on
  **both read and write**. Reads (get-one, list, expansion) and writes
  (update/delete and lifecycle transitions such as `restore`) agree: a caller can
  only write a hidden-state record they can see. This resolves the issue's
  read/write disagreement without a breaking change to unset schemas.

The consequence to design around: once `preview` is set, a caller who holds
`update` but is not in `preview` can no longer edit a *draft* (they can still edit
published records, which are publicly visible). So `preview` must name everyone
who manages hidden states — that is the point of the rule, not a footgun.

### Trash and restore

`preview` covers trashed rows too, so a preview-eligible owner can list their trash
(`?include_deleted`), fetch a trashed record, and `POST /{id}/restore` it — restore
works because the row is now both visible and writable to them. `ownerScope`
narrows this to their own rows. (A separate draft-yes/trash-no split is additive
later; not built.)

### The shared token is unchanged

The token still grants the full admin view across all collections and remains the
path for machines and shareable preview links. The identity path defaults a list
to **published-only** (opt into other states with `?status=`), while the token
path keeps its "any" default — this falls out naturally, because `visibilityFor`
defaults `status` to `any` only when the token matches.

### Validation

- `public` anywhere in a `preview` rule tree is a schema error — a public preview
  would show every draft to everyone, which is never intended and defeats the read
  vs. preview split (a machine reader with `read` must not gain drafts).
- `preview` on a collection with neither `publishing` nor `soft_delete` is a schema
  error — there are no hidden states to gate.

## Consequences

- The authoring-UI case works with no shared secret in the browser: writers see and
  edit their own drafts, editors get a review queue via `?status=draft`, owners can
  undo their own deletes.
- No new queries: the collection-scope decision is in-memory, the record-scope
  decision compares an already-loaded row, and lists gain a filter, not a round
  trip. Expansion's per-record check is in-memory on already-loaded rows.
- Read and write agree once `preview` is set; unset schemas are untouched.
- Supersedes-in-part ADR-0012: lifecycle visibility is no longer token-only when a
  collection opts into `preview`.
- Out of scope: revision-history reads stay token-gated (`previewDenied`), and a
  separate trash-only rule is deferred.
