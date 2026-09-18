# Extension hooks

Hooks run your own Go code on the write lifecycle of the **generated** collection
endpoints — to validate a business rule, derive a field, or perform a side-effect
atomically with the write. They are opt-in and off by default. See ADR-0031 for the
design and the boundaries.

Hooks are the *in-process* transport: trusted Go code you compile in. Out-of-process
(RPC) and sandboxed (WASM) transports are deliberately not built yet.

## Register

Build a `HookRegistry` and pass it in `gateway.Options`:

```go
hooks := gateway.NewHookRegistry().
    On("orders", gateway.BeforeCreate, requireStock).
    On("orders", gateway.AfterCreate, decrementStock)

engine.Serve(ctx, def, db, addr, logger, gateway.Options{Hooks: hooks}, tls)
```

Events (write lifecycle only): `BeforeCreate` `AfterCreate` `BeforeUpdate`
`AfterUpdate` `BeforeDelete` `AfterDelete`. Delete events fire for both a hard delete
and a soft delete. Hooks for one collection fire in registration order.

## Write a hook

```go
func requireStock(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
    item, err := hc.Store.FindOne(ctx, "items", rec["item_id"].(string))
    if err != nil {
        return nil, err
    }
    if toInt(item["stock"]) < toInt(rec["qty"]) {
        return nil, &gateway.HookError{Status: 409, Code: "OUT_OF_STOCK", Message: "not enough stock"}
    }
    return rec, nil
}
```

- **`Before*`** receives the record about to be written. Return it (optionally
  mutated) to proceed, or an error to reject. A mutated record is what gets written.
- **`After*`** receives the written record. Its returned record is ignored; only an
  error matters. Use it for side-effects.
- **Everything runs in the write's transaction.** A returned error rolls the whole
  write back — the row and any side-effects the hook made. A `Before*` rejection
  means nothing was written.

## The context

| Field | What it is |
|---|---|
| `hc.Principal` | the **verified** caller (`auth.Principal`), read-only |
| `hc.Collection` | the collection being written |
| `hc.Event` | which event is firing |
| `hc.Store` | a narrow store handle (`FindOne`/`Find`/`Create`/`Update`/`Delete`) enrolled in the write's transaction |

A hook runs with server authority for its own `hc.Store` operations. It never sets
`created_by`/`updated_by` — the adapter still stamps those from the verified
identity, so a hook cannot forge who acted.

## Rejecting

Return a `*gateway.HookError` to choose the status: `{Status, Code, Message}`. A zero
`Status` defaults to `422`. Any other (non-`HookError`) error is treated as an
unexpected server fault (`500`), since hooks are trusted code — use `HookError` for
business rejections.

## What hooks may not do

- **Change a response's shape.** Hooks change *values*; the schema owns *shape*, so
  the generated OpenAPI/SDK stay honest. Derived response fields belong to a declared
  field, not a hook. (There is no response/read hook for this reason.)
- **Bypass the store lock.** `hc.Store` is a narrow interface — no raw SQL, no
  migrations.
