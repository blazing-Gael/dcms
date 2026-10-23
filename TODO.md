# DCMS — TODO

Follow-through list for making DCMS production-strong. Check off as you go.
Grouped by priority. Last reviewed: 2026-10-03.

---

## 0. In flight — close out
- [ ] Merge **PR #63** — README positioning blurb
- [ ] Decide **PR #51 / #54** (WASM transport + compile-and-ship ADRs) — merge as
      direction-setting, or close if the plan changed. They are design docs only.
- [x] `v0.1.0-beta.4` tagged; #60 + #62 merged

## 1. Credibility — README claims vs. reality  ⚠️ do before the demo video
The feature table / client snippets advertise things not in the code. A technical
viewer will test exactly these. Build them or soften the README to match.
- [ ] **Semantic search** — table says "Built-in ✓"; `vectorize` is parsed but there
      is **no `/search` endpoint and no embedding pipeline wired**. Either wire it or
      mark *planned*.
- [ ] **Wasm plugins** — "Plugins in any language ✓ (Wasm)" + Rust example, but there
      is **no wazero/wasm in the code**; hooks are Go-only. Mark *planned* (ties to #51/#54).
- [ ] **Full typed SDK / web components / `cdn.dcms.dev`** — codegen is **TypeScript
      types only**. Remove or mark planned the `<dcms-collection>` and CDN snippets.
- [ ] **`unix://` socket + `locale: 'ar'`** — neither exists. Remove from client examples.

## 2. Production-blocking for real client ecom
- [ ] **Postgres adapter** — SQLite-only today (single-writer ceiling). The headline
      production item; also forces the change-feed cursor revisit (see §3).
- [ ] **Accounts Phase 2 — email password reset** — Phase 1 done; reset-by-email flow
      still pending. Any store with customer accounts needs it.

## 3. Deferred by design — do as needed
- [ ] **Config secret interpolation** — secrets are env-only until file interpolation
      lands. Fine for now; document the limitation.
- [ ] **Events: bulk replay** — deferred.
- [ ] **Change-feed cursors** — currently UUIDv7 id; revisit for Postgres (do WITH §2).
- [ ] **Media M2–M4** — image transforms / derived renditions.
- [ ] **Generic OIDC (Layer 2)** — bring-your-own auth works out-of-tree; built-in
      verifier deferred.
- [ ] **GraphQL & MCP surfaces** — intentionally derived/optional, not core.

## 4. Documentation (see below / separate docs project)
- [ ] Reconcile README to shipped reality (overlaps §1)
- [ ] Refresh `docs/HOOKS.md` — thin (76 lines) vs. how central hooks now are
- [ ] A real "Usage / Getting Started" guide (quickstart → first collection → auth →
      deploy), currently scattered across README + example
- [ ] Contributor architecture guide (CONTEXT.md is close; add a map of `internal/`)
- [ ] Stand up the official docs site (static HTML + Pagefind)

---

### Suggested order to "strong"
1. Fix README claims (§1) — fastest credibility win, removes the demo landmine
2. Postgres adapter (§2) — headline production item
3. Email password reset (§2) — completes the accounts story
4. Pick ONE of {Wasm plugins, full non-TS SDK} and actually finish it
