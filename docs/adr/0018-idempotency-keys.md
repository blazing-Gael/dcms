
# ADR-0018 — Idempotency keys for unsafe writes

Status: Accepted
Date: 2026-09-01

## Context

The last item of the "M-A · correctness & hardening" milestone is safe retries.
The failure it addresses is concrete and central to the ecom track: a client
`POST`s an order, the response is lost to a dropped connection or a timeout, the
client retries — and a second order is created. Payments, sign-ups, and "place
order" are exactly the actions users double-submit, and a reliability-pitched CMS
cannot turn a network blip into a duplicate charge.

`PATCH`, `PUT`, and `DELETE` are already naturally idempotent — applying them
twice converges to the same state. `POST` (create) is the one unsafe verb: each
call is meant to produce a new row, so a retry can't be distinguished from a
genuine second create *by the server alone*. The client is the only party that
knows "this retry is the same intent as before." So the client must say so, with
a key.

Two hard constraints frame the design:

- **The store interface is locked (ADR-0003)** — no new methods. Whatever we
  build must express itself through the existing `Create`/`FindOne`/`Tx`/`RawExec`
  surface and the engine-managed-collection pattern (`_media`, `_users`,
  `_sessions`).
- **Performance is not negotiable** — the standing mandate is that no one ever
  notices a regression after adopting DCMS. So the cost of idempotency must fall
  *only* on requests that opt into it, and reads and non-keyed writes must pay
  essentially nothing.

## Decision

### 1. Opt-in per request via an `Idempotency-Key` header, on `POST` creates

A client makes a create safe to retry by sending a unique key it generated (a
UUID is the expected shape):

```http
POST /api/v1/orders
Idempotency-Key: 5f3c…-uuid
Content-Type: application/json

{ "total_amount": "42.00", "currency": "USD", … }
```

No header → nothing changes: the request is processed exactly as today, and the
feature costs one "is the header present?" check. The header is honored only on
`POST` create routes of the collection API. It is ignored on the already-safe
verbs, on `/auth`, and on the media byte path. Transitions
(`/publish` etc.) are convergent (publishing twice = published) and are out of
scope; they may adopt the same mechanism later if a need appears.

A present-but-malformed key (empty, or longer than 255 bytes) is a `400` — a
client asking for exactly-once with a broken key should be told, not silently
given at-least-once.

### 2. Durable storage: an engine-managed `_idempotency` collection

Exactly-once across a retry that may cross a process restart or a redeploy
*requires durability* — an in-memory record is defeated by the very crash a retry
is recovering from. The state lives in an engine-managed collection, injected like
`_sessions` (leading underscore → not JSON-CRUD routable), so it rides the normal
migrator and is reached through the **locked** store — no new method:

| field           | type       | purpose                                             |
|-----------------|------------|-----------------------------------------------------|
| `key`           | string, unique | `hash(principal_id ‖ raw-key)` — the dedupe key |
| `fingerprint`   | string     | `hash(method ‖ path ‖ canonical body)`              |
| `status`        | enum       | `in_progress` \| `done`                             |
| `response_code` | integer    | the HTTP status to replay                           |
| `response_body` | text       | the exact JSON body to replay                       |
| `expires_at`    | datetime   | TTL horizon (default now + 24h)                     |

The stored `key` folds in the **principal id** (ADR-0016), so one caller's key can
never collide with another's — a shared key space would let user B's reused UUID
read back user A's created record. Anonymous callers share the anonymous bucket,
which is acceptable because anonymous creates are already access-gated per
collection.

### 3. Semantics: reserve → execute → finalize, all in one transaction

The whole point is that the *duplicate* never executes, so the dedupe record and
the business write must be decided atomically. On a keyed `POST`:

1. **Fast-path read** (outside any transaction): `FindOne` the `_idempotency` row
   by `key`.
   - **`done`, fingerprint matches** → replay: return the stored `response_code`
     and `response_body` verbatim, with an `Idempotent-Replay: true` header. The
     business logic never runs.
   - **fingerprint differs** → `422`: the same key was reused with a *different*
     body. That is client misuse; we neither replay the old response nor execute
     the new one.
   - **`in_progress`** → `409`: an identical request is still running (or died
     mid-flight). The client retries after a moment.
   - **absent / expired** → proceed to step 2.
2. **Inside the business `Tx`:** `INSERT` the `_idempotency` row as
   `in_progress`. Its `UNIQUE(key)` constraint is the concurrency gate — if a
   racing request inserted between our step-1 read and here, this insert fails
   with `ErrConflict`; we roll back and re-resolve via step 1 (replay or `409`).
   Otherwise, still in the same transaction, create the record, serialize the
   response, and `UPDATE` the row to `done` with `response_code`/`response_body`.
   Commit.
3. Return the freshly created response.

Because the reserve, the create, and the finalize commit together, a duplicate
can only ever observe one of two consistent states: "still in progress" or "done
with this exact response." On SQLite the single writer serializes competing
transactions naturally, so the second request simply waits, then finds the
finalized row; a future Postgres adapter gets the same guarantee from the unique
constraint under its normal isolation.

### 4. Retention and cleanup

Keys are not kept forever — `expires_at` defaults to 24h (configurable), which
comfortably covers a client's retry window without letting the table grow without
bound. An expired row is treated as absent on read, and expired rows are swept
periodically (the same lightweight background sweep the engine will use for
expired `_sessions`). A row stuck `in_progress` because its process crashed is
reclaimed when it expires; until then a retry of that specific key gets `409`,
which is the safe answer.

### 5. Configuration

```yaml
server:
  idempotency:
    enabled: true      # default; when false the header is ignored
    ttl_hours: 24
```

Enabled by default: it is inert until a client actually sends a key, so the
default-on posture costs nothing for clients that don't use it while making the
guarantee available to those that do.

## Consequences

- **Retries are safe for the operations that matter** — place-order, sign-up,
  pay — without the application building its own dedupe table. A lost response is
  recoverable: the retry returns the *original* `201` and body, so the client
  still learns the created id.
- **The cost falls only where the guarantee is asked for.** A create with no
  header pays a single boolean check. A keyed create pays one indexed `FindOne`
  plus, within its existing write transaction, one `INSERT` and one `UPDATE` — two
  extra writes, on opted-in POSTs only. **Reads, and every non-keyed write, pay
  nothing.** This is the trade-off put up for explicit sign-off under the
  "never notice a regression" mandate: bounded, opt-in, and off the read path.
- **No store-interface change** — `_idempotency` is another engine-managed
  collection reached through the locked surface, consistent with `_sessions` and
  ADR-0003. A new adapter inherits idempotency for free.
- **A behavior the client must get right:** reusing a key with a *different* body
  is a `422`, and a key is scoped per principal. Both are surfaced in the docs and
  the OpenAPI description of the header.
- **Accepted limitations:** (a) a crashed in-flight request blocks *its own* key
  with `409` until TTL — safe, but not instantly self-healing; (b) idempotency
  covers single-record creates, not the multi-step nested-create tree beyond its
  own transaction boundary (the whole tree commits or rolls back as one, so it is
  still all-or-nothing, but a partial-then-retry resumes as a fresh create unless
  keyed); (c) only `POST` creates are covered in this milestone.

## Alternatives considered

- **An in-memory seen-key cache.** Rejected: not durable, so a retry after a
  restart or redeploy — precisely when retries happen — double-executes. It also
  wouldn't survive across replicas if the topology ever grows.
- **A new `store.Idempotent`/`Reserve` method.** Rejected: violates the locked
  store (ADR-0003). The engine-managed collection plus the existing `Tx` +
  `Create` + `UNIQUE` constraint expresses the same thing with zero interface
  surface.
- **Implicit idempotency from a body hash (no client key).** Rejected: two
  legitimately identical creates — two identical line items, two "add 1 unit"
  clicks the user *means* as two — would silently collapse into one. Only the
  client knows whether a second identical request is a retry or a new intent, so
  the client must mint the key.
- **Store only a "seen" marker, not the response.** Rejected: a retry would then
  get a bare `409` instead of the original `201` + body, so a client that lost the
  first response could never recover the created id. Replaying the stored response
  is what actually makes the retry safe.
- **Lean on natural unique business constraints instead.** Rejected: not every
  create has a natural unique key, a `409` from a constraint doesn't return the
  original resource, and it pushes the burden back onto every schema author.

## Rollout

Implementation is gated on acceptance of this ADR. When accepted: inject the
`_idempotency` collection; thread the reserve/finalize through `handleCreate`
(both the plain and nested-transaction paths already exist); add the config +
env knobs; document the header in SCHEMA_SPEC and the generated OpenAPI; test the
replay, the different-body `422`, the concurrent `409`, and the post-expiry
re-execution. This closes M-A; the Accounts milestone follows.
