# ADR-0001: Schema is the single source of truth

- **Status:** Accepted
- **Date:** 2026-06-20

## Context

Every CMS forces a choice between managed-and-locked-in and
flexible-but-build-everything. DCMS aims to give a production backend from a
single declarative definition, and to keep endpoints, migrations, types,
validation, and docs from ever drifting apart.

## Decision

A single `dcms.schema.yaml`, parsed into `SchemaDefinition`, is the **one source
of truth**. Everything else is *derived* from it: database tables/migrations,
HTTP routes, request/response validation, the OpenAPI spec, documentation, and
generated SDKs. **If it is not in the schema, it does not exist in the API.**

## Consequences

- No drift: a single change propagates to every derived artifact.
- Generators (OpenAPI, SDKs, docs) are pure functions of the parsed schema, so
  they're testable and deterministic.
- The schema language and the parsed IR must be rich enough to express
  everything downstream needs — this raises the bar on schema design.
- Requires discipline: never hardcode field names in generated output; never let
  a derived artifact carry information the schema doesn't.

## Alternatives considered

- **Code-first** (define Go/TS structs, derive schema): ties the product to one
  language and contradicts "developers never touch Go."
- **Hand-maintained spec + code**: guarantees eventual drift.
