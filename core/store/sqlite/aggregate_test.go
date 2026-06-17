package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

func seedProducts(t *testing.T, a *Adapter, recs ...store.Record) {
	t.Helper()
	for _, r := range recs {
		if _, err := a.Create(context.Background(), store.WriteInput{Collection: "products", Data: r}); err != nil {
			t.Fatalf("seed Create: %v", err)
		}
	}
}

func TestAggregate_CountAndSumAndAvg(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A", "price": 10.0},
		store.Record{"title": "B", "price": 20.0},
		store.Record{"title": "C", "price": 30.0},
	)

	count, err := a.Aggregate(ctx, store.AggregateQuery{Collection: "products", Metric: store.Count})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(count) != 1 || count[0].Value != 3 {
		t.Fatalf("count: got %#v, want single Value=3", count)
	}

	sum, err := a.Aggregate(ctx, store.AggregateQuery{Collection: "products", Metric: store.Sum, Field: "price"})
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum[0].Value != 60 {
		t.Fatalf("sum: got %v, want 60", sum[0].Value)
	}

	avg, err := a.Aggregate(ctx, store.AggregateQuery{Collection: "products", Metric: store.Avg, Field: "price"})
	if err != nil {
		t.Fatalf("avg: %v", err)
	}
	if avg[0].Value != 20 {
		t.Fatalf("avg: got %v, want 20", avg[0].Value)
	}
}

func TestAggregate_GroupBy(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A", "stock": int64(5)},
		store.Record{"title": "B", "stock": int64(5)},
		store.Record{"title": "C", "stock": int64(0)},
	)

	res, err := a.Aggregate(ctx, store.AggregateQuery{
		Collection: "products",
		Metric:     store.Count,
		GroupBy:    "stock",
	})
	if err != nil {
		t.Fatalf("group count: %v", err)
	}

	counts := map[int64]float64{}
	for _, r := range res {
		stock, ok := r.GroupValue.(int64)
		if !ok {
			t.Fatalf("GroupValue not int64: %#v (%T)", r.GroupValue, r.GroupValue)
		}
		counts[stock] = r.Value
	}
	if counts[5] != 2 || counts[0] != 1 {
		t.Fatalf("group counts: got %#v, want {5:2, 0:1}", counts)
	}
}

func TestAggregate_WithFilter(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A", "price": 10.0},
		store.Record{"title": "B", "price": 100.0},
	)

	res, err := a.Aggregate(ctx, store.AggregateQuery{
		Collection: "products",
		Metric:     store.Count,
		Filters:    []store.Filter{{Field: "price", Operator: store.Gte, Value: 50.0}},
	})
	if err != nil {
		t.Fatalf("filtered count: %v", err)
	}
	if res[0].Value != 1 {
		t.Fatalf("filtered count: got %v, want 1", res[0].Value)
	}
}

func TestAggregate_SumRequiresField(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	_, err := a.Aggregate(ctx, store.AggregateQuery{Collection: "products", Metric: store.Sum})
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("sum without field: got %v, want ErrInvalidInput", err)
	}
}

func TestRawQuery_TranslatesPlaceholders(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "Cheap", "price": 5.0},
		store.Record{"title": "Pricey", "price": 50.0},
	)

	// Postgres-style $1 must be translated to SQLite's ?.
	rows, err := a.RawQuery(ctx, `SELECT title FROM products WHERE price > $1;`, 10.0)
	if err != nil {
		t.Fatalf("RawQuery: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "Pricey" {
		t.Fatalf("RawQuery result: got %#v, want one row titled Pricey", rows)
	}
}

func TestRawExec_ReturnsRowsAffected(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A", "price": 1.0},
		store.Record{"title": "B", "price": 1.0},
	)

	n, err := a.RawExec(ctx, `UPDATE products SET price = $1 WHERE price = $2;`, 2.0, 1.0)
	if err != nil {
		t.Fatalf("RawExec: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows affected: got %d, want 2", n)
	}
}
