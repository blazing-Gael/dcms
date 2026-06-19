# ADR-0002: Everything configurable except audit/trail/timestamp essentials

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

DCMS's value is sovereignty and flexibility. Locking users into one pattern
(an id strategy, an auth model, an adapter) contradicts that. But some things
must be guaranteed for accountability and compliance.

## Decision

Make nearly everything **configurable / toggleable** — id strategy, auth, RBAC,
storage adapter, embedding provider, etc. — with sensible defaults. The **only
forced, non-optional essentials** are **audit logs, audit trails, and
timestamps** (and the actor attribution that powers them).

Mechanism vs. policy: lower layers provide mechanism (e.g. the store stamps a
column *if it exists*), and the schema/config layer decides policy (which
columns exist) — except for the essentials, which the compiler always emits.

## Consequences

- Strong adaptability; users adopt DCMS without abandoning their patterns.
- Configuration surface and test matrix grow; defaults must be well-chosen.
- Compliance/accountability is guaranteed regardless of configuration.
- Future built-ins to add under this tenet: query/statement timeouts, rate
  limits, and billing.

## Alternatives considered

- **Opinionated/locked design**: simpler to build and test, but limits adoption
  and contradicts the product thesis.
