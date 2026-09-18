# ADR-0032 — WASM plugin transport: drop-in, sandboxed, polyglot extensions

Status: Proposed
Date: 2026-09-18

## Context

ADR-0031 gave DCMS an extension contract — write-lifecycle hooks and custom routes
over a narrow, capability-scoped store interface — and one transport for it:
**in-process Go, compiled in.** That serves the embedder who runs DCMS as a Go
library. It does **not** serve the primary hosting model:

> Install the prebuilt DCMS binary on a box, `dcms init`, `dcms serve`, and add
> custom logic by **dropping plugin files in a folder** — no DCMS rebuild, no
> toolchain on the box, no dependencies to install.

Go is statically compiled, so a prebuilt binary cannot load a `.go` file at runtime,
and Go's `plugin`/`.so` mechanism is a dead end (platform-limited, version-locked,
can't unload). Two options remain for folder-dropped extensions: an **embedded
interpreter** (JS via goja, or Go via yaegi) or **WebAssembly**. We evaluated both
and choose WASM as the *single* dropped-plugin transport.

Why WASM over an embedded interpreter, given we do not want two methods to maintain:

- **Isolation is the correct default for "run code dropped on a box."** A WASM
  module has its own linear memory and no ambient authority — it can only call host
  functions we grant. With fuel/interrupt and memory limits, a runaway or crashing
  plugin cannot take the server down. An in-process interpreter (goja) shares the
  process and needs bolted-on guards. For a server that runs code it did not author,
  this containment is a feature, not overhead.
- **Polyglot without a language lock.** Any language that targets WASM (Rust, Go via
  TinyGo, AssemblyScript, Zig, C, JS via Javy) produces a portable `.wasm`. An
  interpreter fixes one language forever.
- **Marketplace-ready.** The same sandbox that contains a buggy first-party plugin is
  what makes an *untrusted third-party* plugin safe to install — so WASM covers both
  "my own logic" today and a plugin ecosystem later, with no second mechanism.
- **Single static binary preserved.** The runtime, **wazero**, is pure Go with no
  CGO, so the shipped DCMS binary stays one self-contained file with no runtime
  dependencies — the operator's hosting requirement.

The cost is real and accepted: more host-side engineering (the ABI and the guest
SDKs) and an author-side compile step. We judge it worth paying once to avoid
maintaining two runtimes and to get isolation + polyglot from day one.

## Decision

**WASM is the single transport for dropped-in plugins.** It implements the ADR-0031
extension contract over a wasm module boundary; the in-process Go transport remains
for library embedders. Same events, same capability model, two transports.

### Runtime and discovery

- **wazero** (pure Go, no CGO) embedded in the DCMS binary.
- On `serve`, DCMS loads every `*.wasm` under a configured plugin directory
  (default `./hooks`), instantiates each, and registers what it declares.

### The ABI (v1: JSON over linear memory)

WASM passes only numbers and shares linear memory, so structured data crosses as
length-prefixed JSON bytes written into the module's memory. v1 hand-rolls this
convention rather than adopting the WASM Component Model / WIT, whose Go-host tooling
is still maturing (revisit in a later ADR).

- **Host functions (imports we grant)** — the capability surface, exactly the
  ADR-0031 `HookStore`, marshaled: `dcms_find_one`, `dcms_find`, `dcms_create`,
  `dcms_update`, `dcms_delete`, plus `dcms_log`. Nothing else — no filesystem, no
  network, no clock beyond what we expose. Least privilege by construction.
- **Guest exports (functions we call)** — one dispatch entrypoint the module
  implements, keyed by event: the write-lifecycle events (`before_create` … 
  `after_delete`) and custom-route handlers. A `Before*` returns the (possibly
  mutated) record or a rejection; the semantics match ADR-0031 exactly, including
  running inside the write transaction (the host drives the Tx; host-function writes
  the plugin makes enroll in it).

### Isolation limits

Each invocation runs with a **fuel/interrupt budget and a memory cap**, tied to the
existing per-request timeout. A plugin that loops, allocates without bound, or traps
fails that one request (a 5xx the operator sees in logs) and never destabilizes the
server or other plugins.

### Identity and trust

A plugin receives the **verified** principal (read-only) exactly as an in-process
hook does; it cannot set `created_by`/`updated_by` (the adapter still stamps from the
verified identity). The sandbox bounds *resources and crashes*; it does not vouch for
a plugin's *intent*. Installing a plugin — including one copied from OSS — still means
trusting its author with the capabilities it is granted. What the sandbox guarantees
is that a bad plugin cannot exceed those capabilities or take the box down.

### DX deliverables (in scope, not afterthoughts)

A raw ABI is unusable directly; the authoring experience *is* the product:

- **Guest SDKs** wrapping the ABI into idiomatic signatures — `before_create(ctx, rec)`
  — shipping **Rust and TinyGo** first, AssemblyScript next.
- **Scaffolding** — `dcms plugin new --lang rust <name>` generates a ready-to-build
  project (target configured, a stub hook, the build command), removing the
  wasm-toolchain-setup cliff.
- **Typed bindings from the schema** — DCMS already generates TypeScript from the
  schema (contract-type-safety strategy); it generates typed guest structs the same
  way, so a plugin author gets compile-checked `order.Qty`, not `rec.Int("qty")`. This
  is the differentiator that makes WASM DX *better* than an untyped interpreter.
- **`serve --watch <dir>`** rebuild-and-reload in dev, to soften the compile edit-loop.

## Non-goals / deferred

- **No embedded JS/Go interpreter.** WASM satisfies the whole matrix (drop-in,
  no-deps, polyglot, isolated, business logic + routes, trusted and untrusted), so a
  second runtime is not built — one method, per the "don't overcomplicate" tenet.
- **WIT / Component Model** — the cleaner interface story; deferred until the Go-host
  tooling stabilizes. v1 is the hand-rolled JSON ABI.
- **A marketplace / plugin registry / license enforcement** — the ecosystem layer.
  The sandbox + capability model make it *possible* later; this ADR ships the runtime
  and the authoring path, not distribution.
- **Python/other heavy targets** — as their wasm toolchains mature.

## Consequences

- The box-hosting model is fully served: prebuilt binary, drop `*.wasm` in `./hooks`,
  `serve` runs them — no rebuild, no deps, and a bad plugin can't crash the box.
- **One contract, two transports.** A plugin (wasm) and a compiled-in hook (Go)
  register the same events over the same capability surface; ADR-0031's narrow
  `HookStore` is precisely what the ABI serializes — the reason that interface was
  designed narrow in the first place.
- **The SDKs + scaffolding + typed codegen are part of the commitment**, per
  supported language. "Embed wazero" is a fraction of the work; the authoring
  experience is the rest, and skipping it is where plugin adoption dies.
- Custom routes (ADR-0031 §5) are available to plugins too, via a route-handler
  export — so a plugin can add whole endpoints, not just augment writes.
