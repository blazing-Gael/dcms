package schema

import "testing"

func TestParseDecimalExact(t *testing.T) {
	cases := []struct {
		in    string
		scale int
		want  int64
	}{
		{"12.50", 2, 1250},
		{"12.5", 2, 1250},
		{"12", 2, 1200},
		{"0.01", 2, 1},
		{".5", 2, 50},
		{"-3.00", 2, -300},
		{"+7.25", 2, 725},
		{"1000", 0, 1000},
		{"0.001", 3, 1},
		{"0", 2, 0},
		{"-0.00", 2, 0},
	}
	for _, c := range cases {
		got, err := ParseDecimal(c.in, c.scale)
		if err != nil {
			t.Errorf("ParseDecimal(%q, %d): unexpected error %v", c.in, c.scale, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDecimal(%q, %d) = %d, want %d", c.in, c.scale, got, c.want)
		}
	}
}

func TestParseDecimalRejects(t *testing.T) {
	// Money is never silently rounded, and non-numeric / empty / overflowing input
	// is refused rather than coerced.
	bad := []struct {
		in    string
		scale int
	}{
		{"12.505", 2},                    // more fractional digits than scale
		{"12.5.6", 2},                    // two points
		{"12,50", 2},                     // wrong separator
		{"abc", 2},                       // non-numeric
		{"", 2},                          // empty
		{"  ", 2},                        // blank
		{"1e3", 2},                       // no exponents
		{"99999999999999999999999.0", 2}, // int64 overflow
	}
	for _, c := range bad {
		if _, err := ParseDecimal(c.in, c.scale); err == nil {
			t.Errorf("ParseDecimal(%q, %d): expected error, got nil", c.in, c.scale)
		}
	}
}

func TestFormatDecimalRoundTrip(t *testing.T) {
	// Formatting always emits the full declared scale, and parse∘format is identity.
	cases := []struct {
		n     int64
		scale int
		want  string
	}{
		{1250, 2, "12.50"},
		{5, 2, "0.05"},
		{-300, 2, "-3.00"},
		{1000, 0, "1000"},
		{1, 3, "0.001"},
		{0, 2, "0.00"},
	}
	for _, c := range cases {
		got := FormatDecimal(c.n, c.scale)
		if got != c.want {
			t.Errorf("FormatDecimal(%d, %d) = %q, want %q", c.n, c.scale, got, c.want)
		}
		back, err := ParseDecimal(got, c.scale)
		if err != nil || back != c.n {
			t.Errorf("round-trip FormatDecimal→ParseDecimal(%d, %d) = %d (%v), want %d", c.n, c.scale, back, err, c.n)
		}
	}
}

func TestDecimalFloatFreeAddition(t *testing.T) {
	// The bug the type exists to kill: 0.1 + 0.2 != 0.3 in float64, but exact in
	// minor units.
	a, _ := ParseDecimal("0.10", 2)
	b, _ := ParseDecimal("0.20", 2)
	if FormatDecimal(a+b, 2) != "0.30" {
		t.Fatalf("exact addition failed: got %q", FormatDecimal(a+b, 2))
	}
}

func TestDecimalScaleDefault(t *testing.T) {
	if got := (FieldDef{Type: TypeDecimal}).DecimalScale(); got != DefaultDecimalScale {
		t.Fatalf("default scale = %d, want %d", got, DefaultDecimalScale)
	}
	s := 4
	if got := (FieldDef{Type: TypeDecimal, Scale: &s}).DecimalScale(); got != 4 {
		t.Fatalf("explicit scale = %d, want 4", got)
	}
}

func TestValidateDecimalField(t *testing.T) {
	c := CollectionDef{Name: "orders", Fields: []FieldDef{{Name: "total", Type: TypeDecimal}}}

	// A quoted decimal string is accepted.
	if errs := c.ValidateCreate(map[string]any{"total": "12.50"}); errs != nil {
		t.Fatalf("valid decimal rejected: %v", errs)
	}
	// A JSON number is rejected — it is already a lossy float on arrival.
	if errs := c.ValidateCreate(map[string]any{"total": 12.5}); errs == nil {
		t.Fatalf("expected a JSON number to be rejected for a decimal field")
	}
	// Excess precision is rejected, not rounded.
	if errs := c.ValidateCreate(map[string]any{"total": "12.505"}); errs == nil {
		t.Fatalf("expected excess-precision decimal to be rejected")
	}
}

func TestValidateDecimalSchemaScaleAndDefault(t *testing.T) {
	over := 12
	bad := &SchemaDefinition{Version: "1", Collections: []CollectionDef{
		{Name: "a", Fields: []FieldDef{{Name: "x", Type: TypeDecimal, Scale: &over}}},
		{Name: "b", Fields: []FieldDef{{Name: "y", Type: TypeDecimal, Default: "1.234"}}}, // 3 digits > default scale 2
		{Name: "c", Fields: []FieldDef{{Name: "z", Type: TypeDecimal, Default: 5}}},       // non-string default
	}}
	if err := bad.Validate(); err == nil {
		t.Fatalf("expected scale/default validation errors, got nil")
	}

	scale := 3
	good := &SchemaDefinition{Version: "1", Collections: []CollectionDef{
		{Name: "a", Fields: []FieldDef{{Name: "x", Type: TypeDecimal, Scale: &scale, Default: "1.234"}}},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid decimal schema rejected: %v", err)
	}
}

func TestEncodeAndCoerceDecimalRoundTrip(t *testing.T) {
	c := CollectionDef{Name: "orders", Fields: []FieldDef{{Name: "total", Type: TypeDecimal}}}

	// Write encode: string → int64 minor units.
	data := map[string]any{"total": "12.50"}
	c.EncodeDecimals(data)
	if data["total"] != int64(1250) {
		t.Fatalf("EncodeDecimals: got %#v, want int64(1250)", data["total"])
	}

	// Read coerce: whatever integer form the store returns → exact string.
	for _, stored := range []any{int64(1250), 1250, int(1250), float64(1250)} {
		resp := map[string]any{"total": stored}
		c.CoerceResponse(resp)
		if resp["total"] != "12.50" {
			t.Fatalf("CoerceResponse(%T): got %#v, want \"12.50\"", stored, resp["total"])
		}
	}
}
