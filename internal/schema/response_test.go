package schema

import "testing"

func respFixture(t *testing.T) CollectionDef {
	t.Helper()
	def, err := Parse([]byte(`
version: "1"
collections:
  gadgets:
    fields:
      name:   { type: string, required: true }
      active: { type: boolean }
      status: { type: enum, values: [live, paused] }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return def.Collections[0]
}

func TestCoerceResponse_BooleanFromInt(t *testing.T) {
	c := respFixture(t)
	rec := map[string]any{"active": int64(1)}
	c.CoerceResponse(rec)
	if rec["active"] != true {
		t.Errorf("int64(1) should coerce to true, got %#v", rec["active"])
	}
	rec = map[string]any{"active": int64(0)}
	c.CoerceResponse(rec)
	if rec["active"] != false {
		t.Errorf("int64(0) should coerce to false, got %#v", rec["active"])
	}
}

func TestValidateResponse_AcceptsWellFormed(t *testing.T) {
	c := respFixture(t)
	rec := map[string]any{
		"id":         "abc",
		"name":       "widget",
		"active":     true,
		"status":     "live",
		"created_at": "2026-07-02T00:00:00Z",
		"updated_at": "2026-07-02T00:00:00Z",
	}
	if errs := c.ValidateResponse(rec); errs != nil {
		t.Errorf("well-formed record rejected: %v", errs)
	}
}

func TestValidateResponse_CatchesViolations(t *testing.T) {
	c := respFixture(t)
	cases := map[string]struct {
		rec   map[string]any
		field string
	}{
		"missing id":         {map[string]any{"name": "w", "created_at": "2026-07-02T00:00:00Z", "updated_at": "2026-07-02T00:00:00Z"}, "id"},
		"missing timestamp":  {map[string]any{"id": "a", "name": "w", "updated_at": "2026-07-02T00:00:00Z"}, "created_at"},
		"bad enum value":     {map[string]any{"id": "a", "name": "w", "status": "banana", "created_at": "2026-07-02T00:00:00Z", "updated_at": "2026-07-02T00:00:00Z"}, "status"},
		"missing required":   {map[string]any{"id": "a", "created_at": "2026-07-02T00:00:00Z", "updated_at": "2026-07-02T00:00:00Z"}, "name"},
		"wrong boolean type": {map[string]any{"id": "a", "name": "w", "active": int64(1), "created_at": "2026-07-02T00:00:00Z", "updated_at": "2026-07-02T00:00:00Z"}, "active"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errs := c.ValidateResponse(tc.rec)
			if errs == nil {
				t.Fatalf("expected a violation on %q", tc.field)
			}
			if _, ok := errs[tc.field]; !ok {
				t.Errorf("expected error on field %q, got %v", tc.field, errs)
			}
		})
	}
}
