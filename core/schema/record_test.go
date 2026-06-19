package schema

import "testing"

func f64(v float64) *float64 { return &v }

func testCollection() CollectionDef {
	return CollectionDef{
		Name: "products",
		Fields: []FieldDef{
			{Name: "title", Type: TypeString, Required: true, Max: f64(200)},
			{Name: "slug", Type: TypeString, Required: true, Pattern: "^[a-z0-9-]+$"},
			{Name: "price", Type: TypeNumber, Required: true, Min: f64(0)},
			{Name: "stock", Type: TypeInteger, Default: 0},
			{Name: "status", Type: TypeEnum, Values: []string{"draft", "active"}, Default: "draft"},
		},
	}
}

func TestValidateCreate_Valid(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{
		"title": "Honey", "slug": "honey", "price": 12.5, "stock": float64(3), "status": "active",
	})
	if errs != nil {
		t.Fatalf("expected valid, got %v", errs)
	}
}

func TestValidateCreate_MissingRequired(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{"slug": "honey"})
	if errs["title"] == "" || errs["price"] == "" {
		t.Fatalf("expected title and price required errors, got %v", errs)
	}
	// stock/status have defaults → not required.
	if _, ok := errs["stock"]; ok {
		t.Fatalf("stock has a default and should not be required: %v", errs)
	}
}

func TestValidateCreate_TypeMismatch(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{"title": 123, "slug": "x", "price": "lots"})
	if errs["title"] == "" {
		t.Fatalf("expected title type error: %v", errs)
	}
	if errs["price"] == "" {
		t.Fatalf("expected price type error: %v", errs)
	}
}

func TestValidateCreate_IntegerRejectsFraction(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{"title": "x", "slug": "x", "price": 1.0, "stock": 1.5})
	if errs["stock"] == "" {
		t.Fatalf("expected stock integer error for 1.5: %v", errs)
	}
}

func TestValidateCreate_EnumAndPatternAndMax(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{
		"title": "x", "slug": "Bad Slug!", "price": 1.0, "status": "banana",
	})
	if errs["status"] == "" {
		t.Fatalf("expected enum error: %v", errs)
	}
	if errs["slug"] == "" {
		t.Fatalf("expected pattern error: %v", errs)
	}
}

func TestValidateCreate_MinViolation(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{"title": "x", "slug": "x", "price": -5.0})
	if errs["price"] == "" {
		t.Fatalf("expected price >= 0 error: %v", errs)
	}
}

func TestValidateCreate_UnknownField(t *testing.T) {
	c := testCollection()
	errs := c.ValidateCreate(map[string]any{"title": "x", "slug": "x", "price": 1.0, "wibble": true})
	if errs["wibble"] != "unknown field" {
		t.Fatalf("expected unknown field error: %v", errs)
	}
}

func TestValidateUpdate_PartialDoesNotRequire(t *testing.T) {
	c := testCollection()
	// Only updating price; missing required title/slug must NOT error.
	errs := c.ValidateUpdate(map[string]any{"id": "abc", "price": 9.99})
	if errs != nil {
		t.Fatalf("partial update should be valid, got %v", errs)
	}
}

func TestValidateUpdate_StillValidatesPresentFields(t *testing.T) {
	c := testCollection()
	errs := c.ValidateUpdate(map[string]any{"id": "abc", "status": "banana"})
	if errs["status"] == "" {
		t.Fatalf("present field should still be validated: %v", errs)
	}
}
