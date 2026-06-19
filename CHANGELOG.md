# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While on **0.x**, minor versions may include breaking changes.

## [Unreleased]

### Added
- `store` storage abstraction with a pure-Go SQLite adapter: CRUD, eq/comparison/
  `in`/`contains` filters, sort, sparse fieldsets, keyset cursor pagination,
  count/sum/avg aggregation, transactions, and introspection-driven migrations.
- Configurable, injectable id generation (UUIDv7 default).
- Engine-managed audit columns: `created_at`, `updated_at`, `created_by`,
  `updated_by`, with actor attribution via request context.
- Schema parser, validator, and compiler (`dcms.schema.yaml` → tables).
- Virtual REST router: per-collection CRUD, list query params, the standard
  response envelope, and error mapping.
- Server-side request validation (required/type/min/max/pattern/enum).
- OpenAPI 3.1 spec at `/__openapi` and a contract hash (`ETag` / `info.version`).
- Interactive API documentation at `/__docs`.
- Introspection/probe endpoints: `/__schema`, `/__health`, `/__ready`.
- `dcms` CLI: `dev`, `validate`, `migrate`, `version`.

[Unreleased]: https://github.com/blazing-Gael/dcms/commits/main
