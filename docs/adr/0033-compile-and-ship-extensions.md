# ADR-0033 — Compile-and-ship: custom binaries as the primary extension model

Status: Accepted
Date: 2026-09-19

## Context

DCMS extends through hooks and custom routes over one contract (ADR-0031). That
ADR shipped the in-process Go transport (compile-in), and ADR-0032 proposed a WASM
transport for dropping plugins into a folder on a prebuilt binary. The open question
was which is the *primary, blessed* way an operator adds custom logic — and it turns
on how they deploy.

DCMS is tier-2 single-tenant: the operator owns the deployment and the code
(ADR-0007). For that operator there are two shapes:

1. **Stock binary + dropped plugins.** Install the official DCMS binary (there's a
   `curl` install script, Docker image, and Homebrew tap already), then drop plugin
   artifacts in a folder. Requires a runtime plugin loader — WASM (ADR-0032).
2. **Compile-and-ship.** Build a *custom* binary in dev/CI with hooks compiled in
   via the public library (ADR-0031), and ship that binary. The library path (the
   `dcms` module + `App` builder) already makes this work.

The pull toward WASM was the install convenience of (1): a stock binary you `curl`
and never build. But (2) is simpler and more robust on every axis that matters for a
single-tenant, own-the-code product, and the install convenience is recoverable
through ordinary CI. The precedent is Caddy: `xcaddy build --with <module>` compiles
a custom Caddy binary you then ship — a plugin-rich server that deliberately does not
use WASM for first-party extension.

## Decision

**Compile-and-ship is the primary, blessed extension + deployment model.** An
operator builds a custom binary with their hooks (via the ADR-0031 library), and
ships that binary. WASM (ADR-0032) is deferred to a conditional need (below).

### Make it turnkey: `dcms new`

The friction in compile-and-ship is the boilerplate, so a scaffold removes it.
`dcms new <name>` generates a ready-to-build project:

- a Go module (`go.mod`) depending on `github.com/blazing-Gael/dcms`,
- a `main.go` wiring the `App` builder (`dcms.New(...).On(...).Route(...).Serve(ctx)`),
- a `hooks/` package with an example hook and custom route,
- a `schema.yaml` + `dcms.config.yaml`,
- a `Dockerfile` and a **GitHub Actions release workflow** that builds the custom
  binary and publishes a container image (and, via GoReleaser, raw binaries).

The loop becomes: edit a hook → `go build` (or push) → the box `docker pull`s / runs
the image. The box never builds; it pulls a ready artifact.

### Distribution recovers the `curl` convenience

A custom binary is not a DCMS release, but the operator's own CI publishes it:

- **Container:** the box runs `docker pull you/app && docker run` (or compose up) — a
  pull, like `curl`, of the operator's build.
- **Raw binary:** the same workflow's GoReleaser step publishes to the operator's
  own releases, so `curl …/releases/latest/…` works against their artifact.

### Updating DCMS is a pinned dependency bump

Instead of `curl`-ing a new stock binary and hoping it still fits the schema/config,
the operator bumps `go get github.com/blazing-Gael/dcms@vX.Y.Z`, CI rebuilds, the box
pulls. The version is pinned, an incompatibility fails the build loudly, and prod
gets exactly what was tested.

## Why this is better (for the target case)

- **Dev == prod parity.** The exact binary built and tested in dev runs in prod;
  there is no "loads in dev, plugin fails/differs in prod" failure class.
- **Native performance and type safety.** Hooks are Go — no ABI, no serialization
  boundary, no interpreter — debuggable with standard Go tooling.
- **Simplest runtime.** No embedded WASM engine, host ABI, sandbox, or plugin
  discovery in the shipped binary.
- **Zero new engine machinery.** It already works (ADR-0031); the only new surface is
  a scaffold command, not a runtime.

## When WASM (ADR-0032) is still warranted

Compile-and-ship is Go-only and has a build step (in CI). It does **not** cover, and
WASM remains the answer for, exactly three needs — build WASM when one becomes real:

1. **Zero build anywhere** — a stock binary plus dropped plugin files, no build in the
   pipeline at all.
2. **Polyglot authoring** — plugins written in languages other than Go.
3. **Untrusted third-party plugins** — a marketplace running code the operator did not
   audit (needs the sandbox).

Absent one of these, WASM is machinery for problems this model doesn't have.

## Consequences

- ADR-0032 is **deferred to conditional** — designed, not built, until one of the
  three needs above is concrete. This ADR records that gate.
- A new `dcms new` scaffold and the release-workflow templates are the deliverable;
  the runtime is unchanged.
- The two documented extension paths are now clear: **embed the library and ship a
  binary** (this ADR, primary) for own-code single-tenant deployments; **WASM plugins**
  (ADR-0032, later) for polyglot / no-build / untrusted-plugin ecosystems.
