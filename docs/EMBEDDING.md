# Embedding DCMS

Run DCMS from your own Go program — with your business logic compiled in — instead
of the standalone binary. You get the same config, auth, and endpoints as
`dcms serve`, plus hooks and custom routes. No fork.

This is the path for an app (or an AI agent building one) that wants custom logic:
write the schema and config, add DCMS as a dependency, write hooks and endpoints in
Go, `go build`, run.

## Quick start

```go
package main

import (
	"context"
	"log"

	"github.com/blazing-Gael/dcms"
)

func main() {
	app, err := dcms.New(dcms.Options{
		SchemaPath: "schema.yaml",
		ConfigPath: "dcms.config.yaml", // same file the CLI reads; optional
	})
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()

	app.On("orders", dcms.BeforeCreate, requireStock)
	app.On("orders", dcms.AfterCreate, decrementStock)
	app.Route("POST", "/checkout", checkout)

	log.Fatal(app.Serve(context.Background()))
}
```

```
go mod init myshop && go get github.com/blazing-Gael/dcms
dcms init            # or hand-write schema.yaml + dcms.config.yaml
go run .
```

`Options` mirrors the CLI: `ConfigPath`/`SchemaPath` are loaded exactly as
`dcms serve` loads them (config file → environment → these fields), and `DBPath`,
`Port`, `AutoMigrate`, and `Dev` override individual settings. Everything else —
CORS, rate limiting, media, auth provider, webhooks — comes from the config file.

## Hooks

`app.On(collection, event, fn)` registers a write-lifecycle hook. Events:
`BeforeCreate` `AfterCreate` `BeforeUpdate` `AfterUpdate` `BeforeDelete`
`AfterDelete`. See [HOOKS.md](./HOOKS.md) for the full contract — the mutate/reject
semantics, the transaction guarantees, and the identity rules are identical to the
`gateway` types (re-exported here as `dcms.HookContext`, `dcms.HookError`, etc.).

```go
func requireStock(ctx context.Context, hc dcms.HookContext, rec dcms.Record) (dcms.Record, error) {
	item, err := hc.Store.FindOne(ctx, "items", rec["item_id"].(string))
	if err != nil {
		return nil, err
	}
	if toInt(item["stock"]) < toInt(rec["qty"]) {
		return nil, &dcms.HookError{Status: 409, Code: "OUT_OF_STOCK", Message: "not enough stock"}
	}
	return rec, nil
}
```

Organize many hooks however you like — a `hooks/` package that returns functions,
one file per collection, whatever. They are ordinary Go, so an OSS hook is a file
you paste or a package you import.

## Custom routes

`app.Route(method, path, fn)` mounts a new endpoint **inside** DCMS's middleware
stack — identity is already resolved, and the body cap, timeout, and rate limit
apply. The handler gets a `*dcms.Request`:

```go
func checkout(req *dcms.Request) {
	ctx := req.R.Context()
	// As the caller: the cart must be theirs, by the schema's own rules.
	cart, err := req.Store.Get(ctx, "carts", req.R.URL.Query().Get("cart"))
	if errors.Is(err, dcms.ErrNotFound) {
		http.Error(req.W, "no such cart", http.StatusNotFound)
		return
	}
	// System work the caller couldn't do directly: decrement stock.
	_, err = req.System.Update(ctx, dcms.WriteInput{ /* … */ })
	// … charge, respond …
}
```

| Field | What it is |
|---|---|
| `req.Principal` | the verified caller (zero value = anonymous) |
| `req.Store` | a `ScopedStore` **acting as the caller** — the same rules as the HTTP API |
| `req.System` | the privileged store: no access rules. Use it only for system work |
| `req.Can(action, collection, id)` | "may the caller do this?" — same answer as the HTTP API |
| `req.Param(name)` | a path parameter from the route pattern (`/lessons/{id}`) |

Unlike a hook, a custom route is not already in a transaction — open one via
`req.System` if you need atomic multi-writes. Register routes last; they never
shadow a built-in path.

> **Changed (ADR-0038):** `req.Store` used to be the privileged store. It is now
> scoped to the caller; the privileged one is `req.System`.

## Authorization from Go

The rule engine is available in-process, so a custom route never re-implements
authorization (ADR-0038). Everything here runs the **same code** as the HTTP API.

```go
// May p read this lesson? (the schema's `exists` enrollment rule decides)
ok, err := app.Can(ctx, p, dcms.ActionRead, "lessons", id)

// Read and write as p: read rules, published-only view, field masks,
// write-field rules, validation, hooks, events, revisions.
lesson, err := app.As(p).Get(ctx, "lessons", id)
page, err := app.As(p).List(ctx, dcms.Query{Collection: "lessons"})
note, err := app.As(p).Create(ctx, "notes", dcms.Record{"body": "…"})
_, err = app.As(p).Update(ctx, "notes", id, dcms.Record{"body": "…"}, dcms.IfVersion(3))
err = app.As(p).Delete(ctx, "notes", id) // soft-delete collections: trash; dcms.Purge() to hard-delete

// Filters confining your own (privileged) query to what p may read.
filters, ok, err := app.ReadFilters(ctx, p, "lessons")
```

- **`Can`** actions: `ActionRead`, `ActionCreate` (id ignored), `ActionUpdate`,
  `ActionDelete`, `ActionPublish`, `ActionHistory`. `false` means "not allowed" *or*
  "not found / not visible" — the same 404 policy as HTTP; `err` is a real failure.
- **Errors** (test with `errors.Is`): `ErrNotFound` (absent or hidden), `ErrForbidden`,
  `ErrUnauthenticated`, `*ValidationError`, `ErrVersionConflict`, `ErrReferenced`,
  `*HookError`, `ErrUnknownCollection` (engine collections like `_users` aren't reachable).
- `As(p).List` rejects a filter or sort on a field p can't read (`*ValidationError`,
  "unknown field") and refuses subquery filters, exactly as the HTTP list does.

### Media

```go
// Write a derived file (HLS segment, thumbnail) as p: update rights on the asset,
// extension allowlist, size cap, the owner's quota — as PUT /__media/{id}/files/{path}.
n, err := app.Media().PutFile(ctx, p, assetID, "hls/720p/seg1.m4s", r)

// A signed, credential-free URL after checking p may read the asset (as POST /sign).
u, err := app.Media().SignedURLAs(ctx, p, assetID, "hls/", "hls/master.m3u8", 2*time.Hour)

// System use (e.g. a job emailing a certificate link): no access check.
u, err = app.Media().SignedURL(ctx, assetID, "", "", 24*time.Hour)
```

Signed URLs are path-relative — prefix your public origin.

### Example: an enrollment-gated lesson page

```go
app.Route("GET", "/learn/lessons/{id}", func(req *dcms.Request) {
	ctx := req.R.Context()
	lesson, err := req.Store.Get(ctx, "lessons", req.Param("id")) // not enrolled → ErrNotFound
	if err != nil {
		http.NotFound(req.W, req.R)
		return
	}
	video, _ := lesson["video"].(string)
	playlist, err := app.Media().SignedURLAs(ctx, req.Principal, video, "hls/", "hls/master.m3u8", 2*time.Hour)
	// … render the page with lesson + playlist …
})
```

No enrollment query anywhere: the schema's `exists` rule on `lessons` decides.

### Ordering

The App builds its gateway once — at `Serve`, or at the first `Can`/`As`/`Media`
call — and HTTP and the Go API share it. Register every hook and route before that:
`On`/`Route` afterwards panic rather than silently never running.

## When you'd fork instead

Editing the DCMS tree directly is only needed to change the engine itself — a new
field type, a store adapter, core behavior. For business logic, hooks + custom
routes as a dependency are the supported path; you never modify DCMS to add app
logic.

## Loading hooks without recompiling

This is the **compiled-in** path: hooks are Go, built into your binary. A future
transport will load hooks from a folder at runtime (no rebuild, and other
languages), for operators who can't run a Go build — see ADR-0031 for where that
fits. Today, if you can `go build`, this is the whole story.
