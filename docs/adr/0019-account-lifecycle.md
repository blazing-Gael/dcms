
# ADR-0019 — Account lifecycle (local identity self-service + administration)

Status: Accepted
Date: 2026-09-02

## Context

ADR-0016 shipped the identity spine: a `_users` collection (email, password_hash,
roles), opaque DB-backed `_sessions`, `login`/`logout`/`me`, and a bootstrap
admin. It explicitly parked the account-*lifecycle* operations as "later
increments." To reliably run ecom and a real blog we now need those increments:
customers must self-register and reset passwords, and staff must be able to
manage users and roles at runtime — none of which the bootstrap-admin-only model
covers.

This is the **Accounts** milestone. It is local-identity lifecycle, **not**
external identity — OIDC/JWKS stays deferred (ADR-0016 M4, with/after M-E).

Two model decisions were settled with the maintainer before this ADR:

- **Identity and profile are separate.** `_users` stays a thin,
  auth-source-agnostic *identity* (who can log in). The rich "customer / author /
  member" *profile* is a normal user-defined collection linked to the user by
  id / `created_by`. This keeps domain data off the credential store and keeps a
  future OIDC user (no local password) clean. `password_hash` is already optional
  precisely to allow that.
- **Multi-user-per-account (B2B/org) is deferred.** It is a different product
  shape most projects don't have, and — because of the identity/profile split —
  it is additive later: an `orgs` collection plus a `user ↔ org` membership join,
  with at most one new membership-scoped access-rule kind. Nothing here blocks it.

## Decision

### 1. `_users` stays the thin identity; add a `status` flag

Add an additive `status` field to `_users` (`active` | `disabled`, default
`active`). Login rejects a `disabled` user with the same flat `401` as a bad
password (no distinction leaked). Everything richer than credentials + roles +
status lives in an application collection, not here.

### 2. Two endpoint sets: self-service `/auth/*` and admin `/admin/users/*`

**Self-service** (the authenticated caller acts on *themselves*):
- `POST /auth/register` — self-registration (config-gated; see §4)
- `POST /auth/password` — change own password (`{current, new}`)
- `POST /auth/logout-all` — revoke all of the caller's own sessions
- `POST /auth/forgot` + `POST /auth/reset` — password reset (phase 2, §7–8)

**Admin** (gated by an admin role; see §3):
- `GET /admin/users`, `POST /admin/users`
- `GET /admin/users/{id}`, `PATCH /admin/users/{id}` (name/roles/status),
  `DELETE /admin/users/{id}`
- `POST /admin/users/{id}/logout-all` — force-revoke a user's sessions

`password_hash` is never serialized on any of these (reuse `publicUser`).

### 3. Who may administer users: configurable admin roles

`auth.admin_roles` (default `[admin]`). A caller holding any listed role may use
`/admin/users`. Configurable rather than hardcoded, per the configurability
tenet — a project may govern user administration with whatever role name it
declares.

### 4. Self-registration: off by default, safe defaults

- **Disabled unless `auth.registration.enabled: true`.** Open registration is a
  spam magnet on a blog; it must be opted into.
- **`auth.registration.default_roles`** (e.g. `[customer]`), validated against the
  schema-declared roles. A default role that is also an admin role is a config
  error — a self-registrant can never self-grant administration.
- **Enumeration-safe:** a taken email returns the same generic success as a new
  one (no "email exists" oracle).
- On success a session is issued (auto-login) **unless** email verification is
  required (phase 2), in which case no session is issued until verified.

### 5. Password rules and change/reset semantics

- **Policy:** minimum length (`auth.password.min_length`, default 8), and a hard
  reject of inputs longer than **72 bytes** — bcrypt silently truncates there, so
  a longer passphrase would be weaker than it reads. Applied at every entry point
  (register, change, reset, `dcms admin`).
- **Change:** verify `current`, set `new`, then **revoke all *other* sessions**
  (keep the current one) — a password change should log out other devices.
- **Reset (phase 2):** consumes a single-use token and revokes **all** sessions
  (a reset assumes the account may be compromised).

### 6. Session revocation is immediate — the DB-session payoff

`logout-all`, admin force-logout, and reset all reduce to deleting `_sessions`
rows. Opaque DB sessions (ADR-0016, chosen over JWT for exactly this) make
revocation instant and total, with no blocklist.

### 7. One generic `_auth_tokens` collection for reset/verify/invite (phase 2)

`{token_hash unique, user_id (cascade), purpose (reset|verify|invite),
expires_at, used_at}`, injected like `_sessions`. Tokens are **single-use**,
short-TTL, and **stored hashed** (never the raw token — same discipline as
sessions). One purpose-tagged table serves password reset, email verification,
and admin invites without three schemas.

### 8. Email delivery: a thin `Notifier` seam (phase 2)

DCMS owns the *flow* (token generation, storage, expiry, single-use consumption,
and the endpoints); *delivery* is a pluggable `Notifier` interface with an **SMTP**
implementation and a **dev-log** implementation (prints the link to the console
for local testing). When M-B (events/webhooks) lands, the `Notifier` re-routes
through the durable outbox — no endpoint rework. This lets reset ship without
waiting on M-B, while staying the correct long-term seam.

### 9. Abuse resistance

`register`, `forgot`, and `login` sit under the existing per-IP auth rate-limit
tier (ADR request-hardening), which is the coarse guard. Per-account lockout
(failed-attempt tracking) is a deliberate later increment; rate limiting plus
enumeration resistance on `register`/`forgot` is the first-cut posture. This ADR
does not make rate limiting the *only* eventual defense — it names lockout as the
next step.

## Consequences

- Ecom can onboard and **suspend** customers (status) without deleting their
  order history; a blog can manage authors and roles at runtime; all with local
  identity, no OIDC.
- Identity stays thin and auth-source-agnostic, so OIDC slots in later without
  touching profiles or domain data.
- New durable state: an additive `_users.status`, and (phase 2) `_auth_tokens`
  plus an additive `_users.email_verified_at`.
- **Cost:** every new operation is either authenticated or rate-limited, and all
  are writes — **no read-path cost**, consistent with the performance mandate.
- **Accepted trade-offs:** no per-account lockout yet; email verification is
  optional, not forced; org / multi-user-per-account is deferred.

## Alternatives considered

- **Fold the profile into `_users`.** Rejected — ties domain data to the
  credential store and complicates a future OIDC user; the identity/profile split
  is the enabler, not overhead.
- **Hardcode the `admin` role.** Rejected — configurability tenet; projects
  declare their own role names.
- **Reset via CLI only (no email).** Rejected as the default — ecom customers
  need self-service. The `dcms admin` CLI reset still exists as an operator path.
- **JWT so logout needs no server state.** Already rejected in ADR-0016; Accounts
  is precisely why — instant, total revocation is impossible with stateless JWT
  without reintroducing server state anyway.
- **Build reset only after M-B.** Rejected — it would block Accounts on another
  milestone; the `Notifier` seam decouples the flow from delivery.

## Rollout

- **Phase 1 (no email dependency):** `_users.status` + login check · password
  change · logout-all · the 72-byte/min-length password guard · users admin API
  (CRUD + roles + status + force-logout) · self-registration (config-gated).
- **Phase 2 (email seam):** `_auth_tokens` · `Notifier` (SMTP + dev-log) ·
  forgot/reset · optional email verification.

Phase 1 alone unblocks author management for a blog and customer onboarding +
suspension for ecom. Phase 2 adds the customer self-service reset loop.
