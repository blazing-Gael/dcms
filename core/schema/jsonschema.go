package schema

import "strings"

// obj is a shorthand for building JSON (Schema / OpenAPI) fragments.
type obj = map[string]any

// fieldJSONSchema renders one FieldDef as a JSON Schema (draft 2020-12, which
// OpenAPI 3.1 uses). It derives from the SAME constraints the runtime validator
// enforces — that shared source is what keeps types and validation in lockstep.
func fieldJSONSchema(f FieldDef) obj {
	m := obj{}
	switch f.Type {
	case TypeString, TypeText:
		m["type"] = "string"
	case TypeEnum:
		m["type"] = "string"
		m["enum"] = f.Values
	case TypeNumber:
		m["type"] = "number"
	case TypeInteger:
		m["type"] = "integer"
	case TypeBoolean:
		m["type"] = "boolean"
	case TypeDate:
		m["type"] = "string"
		m["format"] = "date"
	case TypeDateTime:
		m["type"] = "string"
		m["format"] = "date-time"
	case TypeJSON:
		// any JSON value — no "type" constraint
	}

	switch f.Type {
	case TypeString, TypeText:
		if f.Min != nil {
			m["minLength"] = int(*f.Min)
		}
		if f.Max != nil {
			m["maxLength"] = int(*f.Max)
		}
		if f.Pattern != "" {
			m["pattern"] = f.Pattern
		}
	case TypeNumber, TypeInteger:
		if f.Min != nil {
			m["minimum"] = *f.Min
		}
		if f.Max != nil {
			m["maximum"] = *f.Max
		}
	}

	if f.Default != nil {
		m["default"] = f.Default
	}
	if f.Label != "" {
		m["title"] = f.Label
	}
	if f.Hint != "" {
		m["description"] = f.Hint
	}
	return m
}

func readOnlyString() obj   { return obj{"type": "string", "readOnly": true} }
func readOnlyDateTime() obj { return obj{"type": "string", "format": "date-time", "readOnly": true} }

// recordSchema is the response shape: id + declared fields + audit columns, with
// engine-managed fields marked readOnly.
func (c CollectionDef) recordSchema() obj {
	props := obj{"id": readOnlyString()}
	for _, f := range c.Fields {
		props[f.Name] = fieldJSONSchema(f)
	}
	props["created_at"] = readOnlyDateTime()
	props["updated_at"] = readOnlyDateTime()
	props["created_by"] = readOnlyString()
	props["updated_by"] = readOnlyString()
	return obj{"type": "object", "properties": props}
}

// createInputSchema is the POST body shape: an optional client-supplied id plus
// the writeable fields. required = required fields that have no default.
func (c CollectionDef) createInputSchema() obj {
	props := obj{"id": obj{"type": "string"}}
	var required []any
	for _, f := range c.Fields {
		props[f.Name] = fieldJSONSchema(f)
		if f.Required && f.Default == nil {
			required = append(required, f.Name)
		}
	}
	schema := obj{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// updateInputSchema is the PATCH body shape: every writeable field, all optional
// (the id comes from the URL path, not the body).
func (c CollectionDef) updateInputSchema() obj {
	props := obj{}
	for _, f := range c.Fields {
		props[f.Name] = fieldJSONSchema(f)
	}
	return obj{"type": "object", "properties": props, "additionalProperties": false}
}

// pascal converts a snake_case collection name to PascalCase for use as an
// OpenAPI component-schema name (e.g. "order_items" → "OrderItems").
func pascal(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}
