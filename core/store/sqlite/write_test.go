package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

func TestUpdate_PartialAndBumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	created, err := a.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey", "price": 12.5, "stock": int64(3)},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created["id"].(string)

	// PATCH only price; title and stock must be untouched.
	updated, err := a.Update(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"id": id, "price": 9.99},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated["price"] != 9.99 {
		t.Fatalf("price: got %#v, want 9.99", updated["price"])
	}
	if updated["title"] != "Honey" {
		t.Fatalf("title should be untouched: got %#v", updated["title"])
	}
	if updated["stock"] != int64(3) {
		t.Fatalf("stock should be untouched: got %#v", updated["stock"])
	}
	// updated_at is re-stamped on every write. We can't assert it strictly
	// advanced (the OS clock may not tick between two back-to-back calls), but it
	// must be present and never earlier than created_at. RFC3339 UTC strings sort
	// lexicographically, so a string compare is a valid time ordering check.
	ua, _ := updated["updated_at"].(string)
	if ua == "" {
		t.Fatalf("updated_at missing after update: %#v", updated)
	}
	if ca, _ := created["created_at"].(string); ua < ca {
		t.Fatalf("updated_at %q is before created_at %q", ua, ca)
	}
	if updated["created_at"] != created["created_at"] {
		t.Fatalf("created_at must not change: %v -> %v", created["created_at"], updated["created_at"])
	}
}

func TestUpdate_NotFound(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	_, err := a.Update(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"id": "nope", "price": 1.0},
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestUpdate_MissingID_Errors(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	_, err := a.Update(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"price": 1.0},
	})
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestDelete_RemovesRow(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	created, err := a.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created["id"].(string)

	if err := a.Delete(ctx, "products", id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.FindOne(ctx, "products", id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	if err := a.Delete(ctx, "products", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestTx_CommitsOnSuccess(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	err := a.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if _, err := tx.Create(ctx, store.WriteInput{Collection: "products", Data: store.Record{"title": "A"}}); err != nil {
			return err
		}
		_, err := tx.Create(ctx, store.WriteInput{Collection: "products", Data: store.Record{"title": "B"}})
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	page, err := a.Find(ctx, store.Query{Collection: "products"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("Total after commit: got %d, want 2", page.Total)
	}
}

func TestTx_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	sentinel := errors.New("boom")
	err := a.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if _, err := tx.Create(ctx, store.WriteInput{Collection: "products", Data: store.Record{"title": "A"}}); err != nil {
			return err
		}
		return sentinel // should roll back the create above
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error: got %v, want sentinel", err)
	}

	page, err := a.Find(ctx, store.Query{Collection: "products"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("Total after rollback: got %d, want 0", page.Total)
	}
}

// TestTx_AtomicOrderAndInventory is the canonical example from STORE_INTERFACE.md:
// decrementing stock and creating an order must be all-or-nothing.
func TestTx_AtomicOrderAndInventory(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	if err := a.Migrate(ctx, store.MigrationPlan{Up: []string{`
		CREATE TABLE orders (
			id         TEXT PRIMARY KEY,
			product_id TEXT,
			quantity   INTEGER,
			created_at TEXT,
			updated_at TEXT
		);`,
	}}); err != nil {
		t.Fatalf("Migrate orders: %v", err)
	}

	product, err := a.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey", "stock": int64(5)},
	})
	if err != nil {
		t.Fatalf("Create product: %v", err)
	}
	pid := product["id"].(string)

	// Attempt to order 10 when only 5 are in stock — must fail and change nothing.
	err = a.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if _, err := tx.Create(ctx, store.WriteInput{
			Collection: "orders",
			Data:       store.Record{"product_id": pid, "quantity": int64(10)},
		}); err != nil {
			return err
		}
		p, err := tx.FindOne(ctx, "products", pid)
		if err != nil {
			return err
		}
		newStock := p["stock"].(int64) - int64(10)
		if newStock < 0 {
			return errors.New("insufficient stock")
		}
		_, err = tx.Update(ctx, store.WriteInput{Collection: "products", Data: store.Record{"id": pid, "stock": newStock}})
		return err
	})
	if err == nil {
		t.Fatal("expected the oversell transaction to fail")
	}

	// Order must not exist and stock must be untouched.
	orders, err := a.Find(ctx, store.Query{Collection: "orders"})
	if err != nil {
		t.Fatalf("Find orders: %v", err)
	}
	if orders.Total != 0 {
		t.Fatalf("orders after rollback: got %d, want 0", orders.Total)
	}
	p, err := a.FindOne(ctx, "products", pid)
	if err != nil {
		t.Fatalf("FindOne product: %v", err)
	}
	if p["stock"] != int64(5) {
		t.Fatalf("stock after rollback: got %#v, want 5", p["stock"])
	}
}
