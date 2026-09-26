# DCMS ecommerce demo

A working store backend built from one `schema.yaml` plus **one hook** — the whole
"place an order" business rule — written against the public `dcms` package. Use it to
see DCMS end to end, or as the script for a demo video.

What it shows: a generated REST API + OpenAPI + typed SDK, an admin panel, media
uploads, exact money, atomic inventory, oversell/payment rejection that rolls back
cleanly, idempotent order creation, signed webhooks, collection- and field-level
access rules, role-scoped status transitions, optimistic concurrency, and revision
history — with the only hand-written code being `checkout` in `main.go` (~40 lines).

## Run it

Requires Go 1.24+. From this directory:

```bash
cd examples/shop
cp .env.example .env      # the admin credentials + webhook secret (gitignored)
go run .                  # migrates, seeds the admin, serves on http://localhost:8080
```

`main.go` loads `.env` on start, so nothing needs exporting. (A real environment
variable still wins over the file — the secrets are read from the environment, never
from `config.yaml`. To use the shell instead of a file: `set -a; source .env; set +a`.)

In a second terminal, seed a catalog and print ready-to-paste demo commands:

```bash
bash seed.sh you@shop.test shoppass
```

The one binary now serves three things on http://localhost:8080 :
- **/** — the customer storefront (browse, cart, sign up, checkout)
- **/__admin** — the admin panel (catalog, orders, media, users)
- **/api/v1** — the REST API (what both of the above, and the curls below, call)

Reset any time: stop the server and `rm -f shop.db && rm -rf shop-media`.

## The demo, shot by shot

Run these live. The `seed.sh` output has the same commands with real IDs filled in.

**0 — The storefront.** Open **http://localhost:8080/** — browse products, add to
cart, sign up, and check out. It's a plain vanilla-JS SPA (`storefront/`, ~1 file
each of HTML/CSS/JS) hitting the same API; the order you place runs through the
`checkout` hook. Everything below is that same store, from the API and admin side.

**1 — The contract.** Open `schema.yaml`. "Four collections and their rules. No SQL,
no endpoints, no admin code." Open `main.go`. "The only code this store needs: what
*checkout* means."

**2 — It's a real API.** `go run .` is already up. Show it's generated:
```bash
curl -s localhost:8080/__schema | head        # OpenAPI, from the schema
curl -s localhost:8080/api/v1/products         # public storefront read (published only)
```

**3 — Place an order.** (from `seed.sh` output) The server computes the total,
decrements stock, and "charges":
```bash
curl -s -X POST localhost:8080/api/v1/orders -H 'Content-Type: application/json' -d '{
  "customer":"<CUST>","pay_token":"tok_ok",
  "items":[{"product":"<HOUSE_BLEND>","qty":2},{"product":"<GREEN_TEA>","qty":1}]
}'
# → "status":"paid","total":"34.00"   (exact money — integer minor units, no floats)
```
Show the stock dropped: `curl -s localhost:8080/api/v1/products/<HOUSE_BLEND>` → `stock` fell by 2.

**4 — It can't oversell, and failures roll back.** Order more than exists:
```bash
curl -s -i -X POST localhost:8080/api/v1/orders -H 'Content-Type: application/json' -d '{
  "customer":"<CUST>","pay_token":"tok_ok","items":[{"product":"<SINGLE_ORIGIN>","qty":99}]
}'
# → HTTP 409 OUT_OF_STOCK. Re-check stock: unchanged. The order AND the decrement
#   happen in one transaction, so a rejection leaves nothing behind.
```
Same for a declined card (`"pay_token":"declined"` → **402**): no order, no stock change.

**5 — Retries don't double-charge.** Send the same order twice with one key:
```bash
curl -s -X POST localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-42' -d '{
  "customer":"<CUST>","pay_token":"tok_ok","items":[{"product":"<GREEN_TEA>","qty":1}]
}'
# Run it again with the same key → same order back, and stock dropped only once.
```

**6 — The admin panel.** Open **http://localhost:8080/__admin**, sign in as the admin.
Show: the catalog with stock, **upload a product image**, publish a draft (status
badge), and the order that was just placed under Orders. It's generated from the same
schema.

**7 — Webhooks (optional).** Every order emits a signed, retried webhook. Start a
listener first (any HTTP sink on :9000), then place an order and show the delivery:
```bash
# tiny listener
python3 -c "import http.server;http.server.HTTPServer(('',9000),type('H',(http.server.BaseHTTPRequestHandler,),{'do_POST':lambda s:(s.send_response(200),s.end_headers(),print('WEBHOOK:',s.rfile.read(int(s.headers['Content-Length']))))})).serve_forever()"
```

**8 — Close.** "Everything you saw except `checkout` is generated. Payments, tax,
shipping — the parts unique to *this* business — are hooks you write. That's the line:
DCMS gives you the store; you write the rules that make it yours."

## More crucial features — quick beats (pick what fits the audience)

Each is ~20 seconds. Log in for the authenticated ones (`<ADMIN>` = the session
cookie, or just show them in the admin panel):

- **Field-level access.** Supplier cost is admin-only:
  ```bash
  curl -s localhost:8080/api/v1/products/<HOUSE_BLEND>              # public: no "cost" field
  curl -s -b admin.cookies localhost:8080/api/v1/products/<HOUSE_BLEND> | grep cost   # admin: "cost":"6.20"
  ```
- **Role-scoped status changes (value-scoped transitions).** On a *paid* order:
  staff can ship it, only an admin can cancel it.
  ```bash
  curl -i -b staff.cookies -X PATCH .../orders/<ID> -d '{"status":"shipped"}'    # 200
  curl -i -b staff.cookies -X PATCH .../orders/<ID> -d '{"status":"cancelled"}'  # 403
  curl -i -b admin.cookies -X PATCH .../orders/<ID> -d '{"status":"cancelled"}'  # 200
  ```
  (Or show it in the panel: the status dropdown offers *shipped* to staff and
  *cancelled* only to an admin — it never offers an illegal move.)
- **Optimistic concurrency (no lost updates).** Two people editing stock:
  ```bash
  curl -i -b admin.cookies -H 'If-Match: 999' -X PATCH .../products/<ID> -d '{"stock":50}'  # 412 Precondition Failed
  ```
- **Revisions (audit + undo).** Edit a price a couple of times, then:
  ```bash
  curl -s -b admin.cookies localhost:8080/api/v1/products/<ID>/revisions   # full version history
  ```
  Restore an old version from the admin panel's **History** view.
- **Typed SDK, generated from the schema.**
  ```bash
  dcms codegen --config config.yaml --out ./types      # → types/dcms-client.ts
  ```
  Open it: `Orders.status` is `'pending' | 'paid' | 'shipped' | 'cancelled'`,
  `items` is a typed array — the client can't drift from the backend.
- **Drafts stay private.** A product with status `draft` is invisible to the public
  storefront read, but staff see it in the admin. (Publishing lifecycle, ADR-0012.)
- **Change feed.** Everything the webhook delivers is also a pollable feed:
  ```bash
  curl -s -b admin.cookies localhost:8080/api/v1/_changes
  ```

## What's core vs. your code

- **Generated from the schema:** collections, REST + OpenAPI, validation, auth &
  access rules, media pipeline, lifecycle/publishing, the admin panel, the change
  feed + webhooks.
- **Yours (the hook):** `checkout` — payment, stock, totals. It runs *inside* the
  order's write transaction, so an order and its inventory effect are always
  consistent. A real store swaps the stubbed payment for a Stripe call.

See `../../docs/HOOKS.md` and `../../docs/EMBEDDING.md` for the hook API.
