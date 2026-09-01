package schema

import (
	"encoding/json"
	"strings"
	"time"
)

// CoerceResponse normalizes a stored record into the JSON shape the schema
// promises, in place. This exists because the store is deliberately
// schema-agnostic (ADR-0003): SQLite has no boolean type, so a `boolean` field
// comes back as an integer 0/1. The gateway — which *does* know the schema —
// is the right place to restore the declared type before it hits the wire, so
// the response matches the OpenAPI spec and the generated SDK types.
//
// Booleans come back as integer 0/1 and are restored to true/false. Structured
// columns (`json` and `richtext`) are stored as JSON text (see the sqlite
// adapter) and are decoded back into objects/arrays here so the response carries
// real structure, not an escaped string. Integers/numbers already serialize
// correctly.
func (c CollectionDef) CoerceResponse(data map[string]any) {
	for _, f := range c.Fields {
		v, ok := data[f.Name]
		if !ok || v == nil {
			continue
		}
		switch f.Type {
		case TypeBoolean:
			if b, ok := numToBool(v); ok {
				data[f.Name] = b
			}
		case TypeJSON, TypeRichText:
			if decoded, ok := decodeJSONColumn(v); ok {
				data[f.Name] = decoded
			}
		case TypeDecimal:
			// Stored as int64 minor units; render back to an exact fixed-scale
			// string at the declared scale (ADR-0017).
			if n, ok := decimalMinorUnits(v); ok {
				data[f.Name] = FormatDecimal(n, f.DecimalScale())
			}
		}
	}
}

// decodeJSONColumn parses a stored JSON column value (text or bytes, as an
// adapter may hand it back) into its structured form. Reports false when the
// value isn't a decodable string/bytes (already structured, or not JSON), leaving
// the caller to keep the original.
func decodeJSONColumn(v any) (any, bool) {
	var raw []byte
	switch s := v.(type) {
	case string:
		raw = []byte(s)
	case []byte:
		raw = s
	default:
		return nil, false
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// numToBool converts the numeric forms a store may hand back into a bool.
func numToBool(v any) (bool, bool) {
	switch n := v.(type) {
	case bool:
		return n, true
	case int64:
		return n != 0, true
	case float64:
		return n != 0, true
	case int:
		return n != 0, true
	}
	return false, false
}

// ValidateResponse checks that a record the server is about to return conforms
// to the schema: engine-managed columns are present and well-formed, and every
// declared field satisfies its type and constraints. It reuses the same
// typeMatches/constraintError checks as request validation (ADR-0008), so the
// server holds its outbound data to the exact contract it holds inbound data to.
//
// Run this AFTER CoerceResponse — it assumes booleans have been restored.
//
// A non-nil result is a server-side contract violation (the store returned data
// that doesn't match the schema — e.g. an enum value written via raw SQL, or a
// column left NULL that the schema marks required), not client error.
func (c CollectionDef) ValidateResponse(data map[string]any) FieldErrors {
	errs := FieldErrors{}

	if id, ok := data["id"].(string); !ok || id == "" {
		errs["id"] = "missing or not a string"
	}
	for _, name := range []string{"created_at", "updated_at"} {
		s, ok := data[name].(string)
		if !ok {
			errs[name] = "missing"
			continue
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			errs[name] = "must be an RFC3339 datetime"
		}
	}

	for _, f := range c.Fields {
		// A many-to-many field has no column on this table (it lives in a join
		// table) and only appears when expanded, so it is never validated here.
		if f.Type == TypeRelation && f.Many {
			continue
		}
		v, present := data[f.Name]
		if !present || v == nil {
			if f.Required {
				errs[f.Name] = "required field missing from response"
			}
			continue
		}
		if !typeMatches(f.Type, v) {
			errs[f.Name] = "must be of type " + string(f.Type)
			continue
		}
		if f.Type == TypeEnum {
			s, _ := v.(string)
			if !containsString(f.Values, s) {
				errs[f.Name] = "must be one of: " + strings.Join(f.Values, ", ")
				continue
			}
		}
		if msg := constraintError(f, v); msg != "" {
			errs[f.Name] = msg
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}
