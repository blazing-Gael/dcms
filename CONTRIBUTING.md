# Contributing to DCMS

Thanks for your interest in DCMS! It's early (v0.x) — the core is taking shape,
and thoughtful contributions and feedback are very welcome.

## Ways to contribute

- **Use it and report friction** — open an issue for bugs, confusing behavior, or
  missing capabilities.
- **Improve docs** — even small clarifications help.
- **Code** — pick up a `good first issue`, or discuss a larger change in an issue
  first so we agree on direction before you invest time.

## Before you start

- For anything non-trivial, **open an issue first** to discuss the approach. This
  saves everyone a wasted PR.
- Changes to the **`store` interface** (`docs/STORE_INTERFACE.md`) require a
  discussion issue — it is a locked contract that every adapter implements.
- Read [`CONTEXT.md`](./CONTEXT.md) for the architecture and the constraints the
  engine must respect.

## Development setup

Requirements: **Go 1.25+**. No C toolchain needed — the SQLite driver
(`modernc.org/sqlite`) is pure Go.

```bash
git clone https://github.com/blazing-Gael/dcms
cd dcms
go build ./...
go test ./...
```

Run the dev server against the example schema:

```bash
go run ./cmd/dcms dev --schema examples/shop.schema.yaml
# then visit http://localhost:3000/__docs
```

Common tasks (see the [`Makefile`](./Makefile)):

```bash
make build   # compile the binary to ./bin
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt
```

## Coding standards

- **`gofmt`** — all code must be formatted (`make fmt`). CI checks this.
- **`go vet` clean** — no vet warnings.
- **Tests required** — new behavior needs tests. We test against in-memory
  SQLite; never mock the storage adapter. See the patterns in
  `core/store/sqlite/*_test.go`.
- **Doc comments** on exported symbols — they become the public pkg.go.dev docs.
- **Respect the layering** — `store` is auth-agnostic and trusts its caller;
  authorization lives at the gateway. Don't leak schema knowledge into the store
  or call the store from a path that bypasses the gateway.
- **Phase discipline** — implement Phase-N features per `docs/DEV_ROADMAP.md`;
  mark deferred work with `// TODO(phase-N):`.

## Architecture decisions

Significant design decisions are recorded as ADRs in [`docs/adr/`](./docs/adr/).
If your change makes or revisits a meaningful decision, add or update an ADR.

## Commit & PR conventions

- Keep commits focused; write clear messages explaining the *why*.
- Reference the issue you're addressing (`Fixes #123`).
- Make sure `go build ./...`, `go vet ./...`, and `go test ./...` all pass.
- PRs run CI automatically; keep it green.

## Reporting security issues

Please do **not** open public issues for vulnerabilities. See
[`SECURITY.md`](./SECURITY.md).

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT License](./LICENSE).
