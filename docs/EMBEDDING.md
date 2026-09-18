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
	if !req.Principal.Authenticated {
		http.Error(req.W, "sign in first", http.StatusUnauthorized)
		return
	}
	cart, err := req.Store.Find(req.R.Context(), /* … */)
	// … write an order, charge, respond …
}
```

`req.Principal` is the verified caller, `req.Store` is the app's store handle. Unlike
a hook, a custom route is not already in a transaction — use `req.Store` for reads
and autonomous writes. Register routes last; they never shadow a built-in path.

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
