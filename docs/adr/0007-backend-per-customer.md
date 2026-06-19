# ADR-0007: Hosted Tier-2 is backend-per-customer, not multi-tenant

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

The hosted offering needs a tenancy model. Logical multi-tenancy (shared DB with
tenant scoping) is operationally lighter but carries isolation and
noisy-neighbor risk.

## Decision

The hosted Tier-2 SaaS gives **each customer a separate, isolated backend** (own
database, own process). DCMS itself is **not** logically multi-tenant, so the
core needs no cross-tenant scoping, `tenant_id` filters, or tenant routing.

A possible *future, per-customer feature* is letting a customer build
multi-tenant websites — that is multi-tenancy *inside one customer's backend*,
not DCMS being multi-tenant.

## Consequences

- Physical data isolation (a strong security/compliance selling point) and
  clean per-instance billing/metering.
- More instances to provision, patch, and monitor — requires solid fleet
  orchestration (handled by the hosting platform).
- Do **not** build shared-schema tenant isolation into the core now.

## Alternatives considered

- **Shared multi-tenant DB**: cheaper to run but weaker isolation, harder
  metering, and broad blast radius for bugs/leaks.
