# ADR-0022 — What belongs in the core, what is a seam, and what is out of scope

Status: Accepted
Date: 2026-09-13

## Context

DCMS keeps accreting integration points — storage (`blob.Store`), account email
(`Notifier`), external identity (`pkg/auth.Authenticator`), events/webhooks. Each
time one appears, the same question recurs: does this feature live *in* the CMS,
does it live *behind a seam* the operator can swap, or is it *not DCMS's job at
all*? Answering it case by case has produced good instincts but no written rule,
so the boundary drifts and every new feature re-litigates it.

Two decisions were explicitly parked pending this: **general/transactional email
sending**, and **OIDC identity-provider support**. This ADR writes down the rule
and settles both.

## Decision

Every feature sorts into exactly one of four buckets. The test is applied in
order; the first match wins.

### 1. Core (inherent, always present)

Ships in core, not optional, opinionated. Qualifies if **any** holds:

- every deployment needs it, or
- it is security-critical (a wrong default is a vulnerability), or
- it defines the product's contract (the shape clients and SDKs depend on).

Examples: the data model and schema compiler, `access:` rules and their
enforcement, audit columns and trails, opaque DB sessions, the account lifecycle
(register / password / reset / logout-all / users admin), idempotency, request
hardening. These are not configurable away (see the configurability tenet:
everything is configurable *except* audit/trails/timestamps).

### 2. Core seam + default implementation (opt-in swap)

Core defines a **small, stable interface** and ships a **safe default** so DCMS
runs out of the box; an operator may replace the implementation. Qualifies if
**all** hold:

- there are multiple legitimate backends,
- DCMS must still work with zero configuration,
- the interface is narrow and stable, and
- the contract is **standards-based, not vendor-specific**.

Examples: `blob.Store` (local disk default → S3/R2/…), `Notifier` (dev-log
default → SMTP), `pkg/auth.Authenticator` (local sessions default → bring your
own). A *generic* OIDC/JWT verifier belongs here too — OIDC is a standard, so a
config-driven verifier (issuer, JWKS, audience, claim→`Principal` mapping) is a
first-class opt-in module, exactly like SMTP: off by default, on via config.

### 3. Out of tree / the application's job

Not in core and not a DCMS-shipped module. Qualifies if **any** holds:

- it is vendor- or provider-specific glue,
- it is application business logic, or
- it is a domain a dedicated service owns better than we would.

The integration point is the seams core already exposes — most often the event
/ webhook system: DCMS emits a fact (`user.created`, `order.published`) and the
application (or a function, or a third-party service) reacts. Examples:
transactional email deliverability, a specific IdP's quirks, business webhooks,
app-specific field types beyond the core set.

### 4. Never

DCMS does not become a competitor to a dedicated provider. It **integrates
with** an email-delivery service, an auth-as-a-service, an object store — it does
not try to *be* one. The moment a feature would pull in deliverability
management, bounce/suppression handling, a template engine, or provider-specific
protocol surface, it has crossed this line.

## The two parked decisions, settled

**General / transactional email sending → bucket 3 (the app's job).** The
`Notifier` seam stays scoped to **account-lifecycle email only** — password reset
today, email verification next, because those are part of the account contract
(bucket 1/2). DCMS will not grow a "send an arbitrary email" API: deliverability,
templating, bounces, and suppression are a product unto themselves (bucket 4).
An application that needs to send business email does so off a DCMS **event** (or
webhook) with its own provider. The durable outbox, retry/backoff, and
dead-letter machinery already built for account email stay internal to that
seam; they are not exposed as a general mail queue.

**OIDC IdP support → the seam is bucket 1 (done: ADR-0020 Layer 1); a generic
OIDC/JWT verifier is bucket 2 (opt-in core module); provider-specific code is
bucket 3 (never shipped).** Prerequisite: an operator must first be able to
*select* an authenticator without forking (GitHub issue #9) — a config-driven
choice, starting with `proxy_header` (trust a verified-identity header behind a
proxy you control) as the smallest unlock. Only after that wiring exists does the
generic OIDC verifier land as a config-enabled module.

## Consequences

- A new integration request is triaged against the four buckets before any code
  is written; the answer is recorded, not re-argued.
- Core stays small and safe-by-default; infrastructure variation lives behind
  narrow, standards-based seams; vendor and app specifics stay out of the tree.
- The event/webhook system (ADR-0021) is the sanctioned escape hatch for
  everything in bucket 3 — which is why it, and not a pile of built-in
  integrations, is the extensibility story.
- This ADR is a lens, not a cage: a feature that genuinely satisfies a higher
  bucket's test moves up. The burden is to show it meets the test, in the ADR
  that introduces it.
