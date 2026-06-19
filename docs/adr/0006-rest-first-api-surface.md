# ADR-0006: REST + OpenAPI is the core; GraphQL & MCP are derived/optional

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

Clients need a primary API. GraphQL is attractive for rich/nested fetching but
carries real costs; AI clients benefit from tool-style access (MCP).

## Decision

**REST + OpenAPI 3.1 is the primary, always-on interface** (auto-generated CRUD
per collection). **GraphQL and MCP are optional layers derived from the same
schema**, added later, not core. **Dashboards** are served by REST building
blocks — aggregation, relation `expand`, sparse fieldsets, and SSE — rather than
requiring GraphQL.

## Consequences

- REST keeps HTTP/CDN cacheability (critical for public content), universal
  consumability, and free client/codegen via OpenAPI.
- GraphQL's costs (lost caching, N+1, query-complexity abuse) are deferred and
  isolated to an opt-in layer if/when added.
- The schema IR must stay rich enough to generate GraphQL SDL / MCP tools later
  at low marginal cost.

## Alternatives considered

- **GraphQL-first**: loses caching and adds operational/security surface for the
  common case.
- **REST-only forever**: fine, but forecloses the AI-native (MCP) wedge; we keep
  the door open instead.
