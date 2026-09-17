package schema

import (
	"maps"
	"strings"
	"testing"
)

// ptr is a tiny helper for the *float64 bound fields.
func fptr(v float64) *float64 { return &v }

// richInnerCollection returns a `pages` collection whose object_list element
// exercises every inner kind that carries a constraint, so the recursive
// validator can be tested against the full matrix in one place.
func richInnerCollection() CollectionDef {
	return CollectionDef{
		Name: "pages",
		Fields: []FieldDef{
			{Name: "slots", Type: TypeObjectList, Min: fptr(1), Max: fptr(3), Of: []FieldDef{
				{Name: "title", Type: TypeString, Required: true, Min: fptr(2), Max: fptr(10)},
				{Name: "code", Type: TypeString, Pattern: `^[A-Z]{3}$`},
				{Name: "qty", Type: TypeInteger, Min: fptr(0), Max: fptr(99)},
				{Name: "rating", Type: TypeNumber, Min: fptr(0), Max: fptr(5)},
				{Name: "kind", Type: TypeEnum, Values: []string{"a", "b"}},
				{Name: "price", Type: TypeDecimal, Scale: intPtr(2)},
				{Name: "on", Type: TypeBoolean},
				{Name: "day", Type: TypeDate},
				{Name: "at", Type: TypeDateTime},
			}},
		},
	}
}

func intPtr(v int) *int { return &v }

// slot wraps a single element into a valid-shaped create body.
func slot(el map[string]any) map[string]any {
	return map[string]any{"slots": []any{el}}
}

func TestObjectList_InnerConstraintMatrix(t *testing.T) {
	c := richInnerCollection()
	base := map[string]any{"title": "OK"} // satisfies the one required inner field

	// Each case overlays one bad inner value and expects an error mentioning `want`.
	cases := []struct {
		name  string
		field string
		val   any
		want  string
	}{
		{"string too short", "title", "x", "at least 2"},
		{"string too long", "title", "waytoolongvalue", "at most 10"},
		{"pattern mismatch", "code", "abc", "invalid format"},
		{"integer not whole", "qty", 1.5, "must be of type integer"},
		{"integer below min", "qty", -1, "must be >= 0"},
		{"integer above max", "qty", 100, "must be <= 99"},
		{"number above max", "rating", 6, "must be <= 5"},
		{"enum not allowed", "kind", "z", "must be one of"},
		{"decimal as number", "price", 12.5, "decimal string"},
		{"boolean wrong type", "on", "yes", "must be of type boolean"},
		{"date malformed", "day", "not-a-date", "must be a date"},
		{"datetime malformed", "at", "not-a-time", "RFC3339"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			el := map[string]any{}
			maps.Copy(el, base)
			el[tc.field] = tc.val
			errs := c.ValidateCreate(slot(el))
			if errs == nil {
				t.Fatalf("expected an error for %s, got none", tc.field)
			}
			if msg := errs["slots"]; !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
		})
	}

	// A fully-valid element with every inner kind populated passes.
	good := slot(map[string]any{
		"title": "Valid", "code": "ABC", "qty": 3, "rating": 4.5,
		"kind": "a", "price": "12.50", "on": true,
		"day": "2026-01-02", "at": "2026-01-02T15:04:05Z",
	})
	if errs := c.ValidateCreate(good); errs != nil {
		t.Fatalf("valid rich element rejected: %v", errs)
	}
}

func TestObjectList_ListLengthBounds(t *testing.T) {
	c := richInnerCollection() // min 1, max 3

	// Too few (min 1) — an empty list violates the minimum.
	if errs := c.ValidateCreate(map[string]any{"slots": []any{}}); errs == nil || !strings.Contains(errs["slots"], "at least 1") {
		t.Fatalf("min length not enforced: %v", errs)
	}
	// Too many (max 3).
	four := []any{}
	for range 4 {
		four = append(four, map[string]any{"title": "ok"})
	}
	if errs := c.ValidateCreate(map[string]any{"slots": four}); errs == nil || !strings.Contains(errs["slots"], "at most 3") {
		t.Fatalf("max length not enforced: %v", errs)
	}
}

func TestObjectList_ElementMustBeObject(t *testing.T) {
	c := richInnerCollection()
	// A scalar where an element object is expected.
	if errs := c.ValidateCreate(map[string]any{"slots": []any{"nope"}}); errs == nil || !strings.Contains(errs["slots"], "must be an object") {
		t.Fatalf("non-object element not caught: %v", errs)
	}
}

func TestObjectList_KeyMustBeString(t *testing.T) {
	c := richInnerCollection()
	el := map[string]any{"_key": 123, "title": "ok"}
	if errs := c.ValidateCreate(map[string]any{"slots": []any{el}}); errs == nil || !strings.Contains(errs["slots"], "_key must be a string") {
		t.Fatalf("non-string _key not caught: %v", errs)
	}
}

func TestObjectList_UpdateValidatesElements(t *testing.T) {
	c := richInnerCollection()
	// PATCH replaces the whole list, so a bad element on update is still rejected
	// (the list isn't a partial merge).
	bad := map[string]any{"slots": []any{map[string]any{"title": "x"}}} // too short
	if errs := c.ValidateUpdate(bad); errs == nil || !strings.Contains(errs["slots"], "at least 2") {
		t.Fatalf("update should validate elements: %v", errs)
	}
	// An update that omits the list entirely is fine (untouched).
	if errs := c.ValidateUpdate(map[string]any{}); errs != nil {
		t.Fatalf("omitting object_list on update should be allowed: %v", errs)
	}
}

func TestObjectList_MinGreaterThanMaxRejected(t *testing.T) {
	src := "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        min: 5\n        max: 2\n        of:\n          y: { type: string }\n"
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "min (5) may not exceed max (2)") {
		t.Fatalf("min>max should fail compile, got %v", err)
	}
}

func TestObjectList_InnerFileManyRejected(t *testing.T) {
	src := "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          gallery: { type: file, many: true }\n"
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "many-to-many") {
		t.Fatalf("inner file/many should fail compile, got %v", err)
	}
}

func TestObjectList_JSONSchemaShape(t *testing.T) {
	f := FieldDef{Name: "slots", Type: TypeObjectList, Min: fptr(1), Max: fptr(4), Of: []FieldDef{
		{Name: "title", Type: TypeString, Required: true},
		{Name: "kind", Type: TypeEnum, Values: []string{"a", "b"}},
	}}
	m := fieldJSONSchema(f)

	if m["type"] != "array" {
		t.Fatalf("type = %v, want array", m["type"])
	}
	if m["minItems"] != 1 || m["maxItems"] != 4 {
		t.Fatalf("min/maxItems = %v/%v, want 1/4", m["minItems"], m["maxItems"])
	}
	items, ok := m["items"].(obj)
	if !ok || items["type"] != "object" {
		t.Fatalf("items not an object schema: %#v", m["items"])
	}
	if items["additionalProperties"] != false {
		t.Fatalf("items should forbid additionalProperties, got %#v", items["additionalProperties"])
	}
	props, _ := items["properties"].(obj)
	if _, has := props[objectListKey]; !has {
		t.Fatalf("items.properties missing %s", objectListKey)
	}
	kind, _ := props["kind"].(obj)
	if enum, _ := kind["enum"].([]string); len(enum) != 2 {
		t.Fatalf("inner enum not carried into items: %#v", kind)
	}
	req, _ := items["required"].([]any)
	if len(req) != 1 || req[0] != "title" {
		t.Fatalf("required = %#v, want [title]", req)
	}
}

func TestObjectList_ResponseCoercionAndValidation(t *testing.T) {
	c := richInnerCollection()

	// A stored JSON-column string (as the adapter hands back) is decoded to an array.
	rec := map[string]any{
		"id": "11111111-1111-7111-8111-111111111111", "created_at": "2026-01-02T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		"slots": `[{"title":"Hi"}]`,
	}
	c.CoerceResponse(rec)
	if _, ok := rec["slots"].([]any); !ok {
		t.Fatalf("CoerceResponse should decode the JSON column to an array, got %T", rec["slots"])
	}
	if errs := c.ValidateResponse(rec); errs != nil {
		t.Fatalf("coerced record should pass response validation: %v", errs)
	}

	// A stored non-array value is a server-side contract violation.
	bad := map[string]any{
		"id": "1", "created_at": "2026-01-02T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		"slots": `{"not":"an array"}`,
	}
	c.CoerceResponse(bad)
	if errs := c.ValidateResponse(bad); errs == nil || errs["slots"] == "" {
		t.Fatalf("non-array stored value should fail response validation, got %v", errs)
	}
}
