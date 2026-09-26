// Command shop is the DCMS ecommerce demo: the generated backend from schema.yaml
// plus one business-logic hook — checkout — written against the public dcms package.
//
// Run it from this directory:
//
//	export DCMS_ADMIN_EMAIL=you@shop.test DCMS_ADMIN_PASSWORD=shoppass
//	export DCMS_WEBHOOK_ORDER_SECRET=devsecret
//	go run .
//
// See README.md for the full walkthrough (seed data, storefront calls, the admin
// panel, and the webhook listener).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	dcms "github.com/blazing-Gael/dcms"
)

func main() {
	app, err := dcms.New(dcms.Options{
		SchemaPath:     "schema.yaml",
		ConfigPath:     "config.yaml",
		ConfigRequired: true,
		AutoMigrate:    true, // create/upgrade tables on start (dev convenience)
		Dev:            true, // strict response validation on
		Label:          "shop",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()

	// The one piece of code this store needs beyond its schema: what "place an order"
	// means. Everything else — CRUD, auth, media, the admin panel — is generated.
	app.On("orders", dcms.BeforeCreate, checkout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := app.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}

// checkout runs inside the order's write transaction (ADR-0031): it takes payment,
// verifies stock, decrements inventory, and computes the exact total. Returning an
// error rolls the whole write back — no order row, no stock change — so an order and
// its inventory effect are always consistent. All money is integer minor units
// (ADR-0017): no floating point ever touches a price.
func checkout(ctx context.Context, hc dcms.HookContext, order dcms.Record) (dcms.Record, error) {
	// 1. Payment (stubbed). A real store would call Stripe/etc. here; a pay_token of
	//    "declined" simulates a failure, which aborts the order.
	if tok, _ := order["pay_token"].(string); tok == "declined" {
		return nil, &dcms.HookError{Status: 402, Code: "PAYMENT_DECLINED", Message: "the payment was declined"}
	}

	items, ok := order["items"].([]any)
	if !ok || len(items) == 0 {
		return nil, &dcms.HookError{Status: 422, Code: "EMPTY_ORDER", Message: "an order needs at least one item"}
	}

	var total int64 // minor units, e.g. 2598 == $25.98
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		productID, _ := item["product"].(string)
		qty := asInt(item["qty"])
		if qty <= 0 {
			return nil, &dcms.HookError{Status: 422, Code: "BAD_QUANTITY", Message: "each item needs a positive quantity"}
		}

		p, err := hc.Store.FindOne(ctx, "products", productID)
		if err != nil {
			return nil, err
		}
		stock := asInt(p["stock"])
		if stock < qty {
			name, _ := p["name"].(string)
			return nil, &dcms.HookError{
				Status:  409,
				Code:    "OUT_OF_STOCK",
				Message: fmt.Sprintf("%q: only %d in stock, ordered %d", name, stock, qty),
			}
		}
		total += asInt(p["price"]) * qty

		// Decrement stock in the SAME transaction as the order — so the two can never
		// disagree, and two concurrent orders can't oversell.
		if _, err := hc.Store.Update(ctx, dcms.WriteInput{
			Collection: "products",
			Data:       dcms.Record{"id": productID, "stock": stock - qty},
		}); err != nil {
			return nil, err
		}
	}

	order["total"] = total   // exact; the API renders it as a decimal string
	order["status"] = "paid" // payment succeeded above
	return order, nil
}

// asInt coerces a stored numeric (int64 from SQLite, or float64 from a JSON column)
// to int64.
func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
