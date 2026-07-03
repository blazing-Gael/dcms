# ADR-0009: Layered configuration — one file, env and flag overrides

- **Status:** Accepted
- **Date:** 2026-07-02

## Context

DCMS is meant to run unchanged across a developer's laptop, CI, staging, and
production. Those environments differ in ways that must *not* require rebuilding
or forking the binary: which database, where it lives, what port, whether auth
is on, which embedding provider, and so on. This is the configurability tenet
(ADR-0002) applied to runtime settings — everything is a knob except the
essentials (audit logs, trails, timestamps).

Phase 1 shipped settings as command-line flags only. That does not scale:

- A production launch becomes a long, typo-prone flag string buried in a
  Dockerfile or systemd unit, with no artifact to review or diff.
- Flags leave no trace in the repo, so "how does this instance run?" becomes
  tribal knowledge.
- Secrets (DB passwords, SSO client secrets, API keys) have nowhere sensible to
  live — you don't want them in a committed file *or* in shell history.
- Every future subsystem (auth, rate limits, AI provider, adapter DSN) would
  otherwise have to invent its own flag surface.

We need a configuration model that is settled *before* those subsystems land, so
each one arrives as "add a struct field," not "retrofit configuration."

## Decision

A single resolved `Config` is assembled from four layers. **Highest precedence
wins:**

```
flags  >  environment variables (DCMS_*)  >  config file  >  built-in defaults
```

Each layer has a distinct job:

- **Defaults** — every setting has one, so the zero-config path (`dcms dev` in a
  directory with a schema) works untouched. An empty config file is valid.
- **Config file** (`./dcms.config.yaml`, or `--config path`) — the baseline an
  instance ships, committed to git, reviewable, diffable. A partial file only
  overrides the keys it names; absent keys keep their default.
- **Environment variables** — per-environment overrides of individual values
  without editing the file (the 12-factor pattern). This is also the channel for
  **secrets**, which must never sit in the committed file.
- **Flags** — one-off, this-run-only overrides, applied only when explicitly set
  (so a flag's own default can't silently clobber the file).

The config file is the single place a user declares *how their instance runs*.
It grows one section per subsystem. The intended shape (only `schema`,
`database`, `server` exist today; the rest are reserved direction):

```yaml
schema: ./dcms.schema.yaml

database:
  driver: sqlite            # sqlite | postgres | couchbase — swap adapter here (ADR-0003)
  path: ./dcms.db           # sqlite file; networked drivers use `dsn:` instead

server:
  port: 3000
  # cors, rate_limit, timeouts — the guardrails against abuse/overload

auth:
  mode: off                 # off | manual | rbac | sso | external
                            # a GATEWAY setting; the store stays auth-agnostic (ADR-0003)

ai:
  embeddings: { provider: ..., model: ... }

id:
  generator: uuidv7         # uuidv7 | uuidv4 | off (ADR-0004)
```

**Boundaries this decision fixes:**

1. **Config selects policy; enforcement lives where it belongs.** `auth.mode`
   configures the *gateway* middleware, not the store. The store never learns
   about auth (ADR-0003); it only receives an already-authorized actor id for
   attribution (ADR-0005). Config picks the policy; the gateway is where it bites.
2. **Adapter choice is a config value, not a code change.** `database.driver`
   exists today and rejects anything but `sqlite` with a loud error, so the slot
   is real before the Postgres adapter exists. This backs the "swap adapters via
   one config line" promise.
3. **The resolver package (`internal/config`) is stable; the schema is additive.**
   New settings are new fields with defaults. They inherit file + env + flag
   support automatically.

## Consequences

- Any future setting gets the full layering for free — we build the resolver
  once, now.
- A DCMS instance's runtime shape is a committed, reviewable artifact. The
  *same* artifact runs across every environment; only env/flags differ.
- **Secrets stay out of the file.** The env layer is the sanctioned channel for
  credentials. We must add env-var *interpolation into* the file
  (`dsn: ${DCMS_DB_DSN}`) before shipping the first secret-bearing setting;
  today env vars override whole keys only. Until then, no setting that carries a
  secret may be file-only.
- A config naming an unimplemented driver (or, later, mode/provider) fails loudly
  at startup rather than being silently ignored — a stale config is a visible
  error, not a mystery.
- Cost: three sources of truth for one value means precedence must be tested and
  documented, or "why isn't my setting taking effect?" becomes a support burden.
  The precedence order is covered by unit tests and stated in the README.

## Alternatives considered

- **Flags only (the Phase 1 state).** Doesn't scale to many settings, leaves no
  reviewable artifact, and has nowhere safe for secrets. Rejected.
- **Config file only, no env/flag layer.** Forces a file edit (and often a
  rebuilt image) for a one-line per-environment change; blocks the 12-factor
  secret pattern. Rejected.
- **Env-vars only (strict 12-factor).** Great for secrets and containers, poor
  for a rich nested surface (adapters, provider blocks) and gives no
  human-readable baseline to commit. We take the useful half — env as an
  override/secret channel — layered over a file.
- **A dynamic key-value store (e.g. Consul) as the source of truth.** Too heavy
  for the sovereign, single-binary, $5-VPS deployment target. A file plus env is
  enough and keeps the "one binary, one config" story intact.
