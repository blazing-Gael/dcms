# ADR-0029 — Passwordless email-OTP login

Status: Accepted
Date: 2026-09-17

## Context

Login is email + password only (ADR-0016). For a consumer audience two common
cases are the wrong shape (GitHub #11):

1. **Apps that shouldn't show a login screen.** A PWA where the account is a
   side effect of something else (a purchase, an invite) wants to hand the user a
   session without ever asking for a password. Today the only route is to mint a
   random password the user never sees — which makes "restore on a new device"
   the *password-reset* flow wearing different copy. A workaround, not a design.
2. **Users who lose passwords.** For a consumer audience (students, in the
   motivating Agrojatra case) reset is not an edge case but a routine path, and
   every one is a support burden and a drop-off point. An emailed code has no
   such failure mode.

Nearly everything needed already exists: `_auth_tokens` has a `purpose` column
(`reset | verify | invite`) and an expiry sweep; the `Notifier` seam with durable
outbox delivery (ADR-0021 phase 3) already sends account mail; sessions are opaque
and issued by `handleLogin`. An OTP flow is a third purpose plus two endpoints.

## Decision

Add opt-in passwordless login by emailed one-time code, reusing the existing
primitives rather than adding new ones.

**Endpoints** (mounted under `/auth` only when enabled):

- `POST /auth/otp/request {email}` → **always 204**, whether or not the account
  exists, so it can't enumerate users (mirrors `POST /auth/forgot`).
- `POST /auth/otp/verify {email, code}` → issues a session identical to a
  password login (opaque token in body + `HttpOnly` cookie) on success; every
  failure is the same flat **401**.

**Token model.** A `login` purpose is added to `_auth_tokens`. The code is a
6-digit numeric string; only its hash is stored, and the hash binds the user id
(`sha256(userID + ":" + code)`) so two users who draw the same code get distinct
hashes — the `token_hash` uniqueness holds — and a leaked hash is useless without
the user id. A new `attempts` column backs the brute-force cap.

**Security model for a low-entropy code.** A 6-digit code has only 10^6 values,
so the safety rests on four things, all enforced server-side:

- **Short TTL** (default 10 min), so the window to guess is small.
- **Single use** — a correct code is deleted before the session is issued.
- **Per-code attempt cap** (default 5): each wrong guess increments the counter,
  and the code is burned when the cap is reached, so the space can't be walked.
  Verify looks the code up by `user_id + purpose` (not by hash), so a wrong guess
  still finds the row and counts against it. Compare is constant-time.
- **Invalidate outstanding codes on a new request** — issuing a fresh code first
  deletes the user's prior `login` tokens, so only the newest is ever live.

**Rate limiting.** The endpoints sit under the per-IP auth tier already. On top of
that, code *requests* are throttled per recipient email (fixed 4/min, burst 2) so
the endpoint can't bomb one inbox from many IPs. A throttled request is still a
generic 204 — it reveals nothing, it just skips minting and sending.

**Opt-in.** Off by default (`auth.otp.enabled`). Enabling it lets anyone with
access to a user's inbox obtain a session, so it is an explicit operator choice;
when off, the routes are not mounted (404).

## Consequences

- The notification outbox gains a `code` column beside `link`; the `login_otp`
  kind renders the code (not a URL). Delivery, retry, and dead-lettering are
  unchanged.
- `purpose: verify` (email verification, still unimplemented) is now nearly free
  and arguably subsumed: a user who logged in with an emailed code has
  demonstrably verified the address.
- **Deferred — magic-link variant.** The issue floated letting the same token be
  consumed via `?token=` as reset does. A high-entropy magic link and a
  human-typed 6-digit code are different artifacts with different threat models
  (a 6-digit code is not safe to put in a URL), so this ADR ships the code flow
  and leaves a link-based passwordless login to a later pass if a use case wants
  it.
