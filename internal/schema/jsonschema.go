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
	case TypeRelation:
		// On input a relation is the target id(s): a string for belongs-to, an
		// array of ids for many-to-many. The response can also hold the expanded
		// object(s) — see relationRecordSchema.
		if f.Many {
			m["type"] = "array"
			m["items"] = obj{"type": "string"}
		} else {
			m["type"] = "string"
		}
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
	case TypeRichText:
		// A structured content document — a shared reusable component (ADR-0014).
		return ref("RichText")
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
	// Field-level access (ADR-0016 M2): read masks the field out of a response for
	// an unauthorized reader; write drops an unauthorized writer's value. Surfaced
	// so generated clients/docs know a field may be absent or ignored per identity.
	if f.Access != nil {
		if f.Access.Read != nil {
			m["x-access-read"] = accessNote(*f.Access.Read)
		}
		if f.Access.Write != nil {
			m["x-access-write"] = accessNote(*f.Access.Write)
		}
	}
	return m
}

func readOnlyString() obj   { return obj{"type": "string", "readOnly": true} }
func readOnlyDateTime() obj { return obj{"type": "string", "format": "date-time", "readOnly": true} }

// relationRecordSchema renders a relation field as it appears in a RESPONSE: the
// target id(s) when not expanded, or the target record(s) when `?expand=` is
// used. The union ("id or object") is expressed with anyOf against the target's
// component schema.
func relationRecordSchema(f FieldDef) obj {
	target := ref(pascal(f.Target))
	if f.Many {
		return obj{
			"type":        "array",
			"items":       obj{"anyOf": []any{obj{"type": "string"}, target}},
			"description": "target ids, or the expanded records when ?expand=" + f.Name + " is used",
		}
	}
	return obj{
		"anyOf":       []any{obj{"type": "string"}, target},
		"description": "target id, or the expanded record when ?expand=" + f.Name + " is used",
	}
}

// relationInputSchema renders a relation field as it appears in a create/update
// body: the target id(s), or an inline record to create (nested write). The
// inline shape references the target's own create-input component.
func relationInputSchema(f FieldDef) obj {
	// Media assets are created by uploading bytes, never as an inline JSON object,
	// so a file/media relation's input is only the id(s).
	if f.Target == MediaCollection {
		if f.Many {
			return obj{"type": "array", "items": obj{"type": "string"}, "description": "media asset ids"}
		}
		return obj{"type": "string", "description": "media asset id"}
	}
	inline := ref(pascal(f.Target) + "CreateInput")
	if f.Many {
		return obj{
			"type":        "array",
			"items":       obj{"anyOf": []any{obj{"type": "string"}, inline}},
			"description": "target ids and/or inline records to create",
		}
	}
	return obj{
		"anyOf":       []any{obj{"type": "string"}, inline},
		"description": "target id, or an inline record to create",
	}
}

// recordSchema is the response shape: id + declared fields + audit columns, with
// engine-managed fields marked readOnly.
func (c CollectionDef) recordSchema() obj {
	props := obj{"id": readOnlyString()}
	for _, f := range c.Fields {
		if f.Type == TypeRelation {
			props[f.Name] = relationRecordSchema(f)
			continue
		}
		props[f.Name] = fieldJSONSchema(f)
	}
	props["created_at"] = readOnlyDateTime()
	props["updated_at"] = readOnlyDateTime()
	props["created_by"] = readOnlyString()
	props["updated_by"] = readOnlyString()
	// Lifecycle-managed columns (ADR-0012), readonly, for whichever directives the
	// collection opted into.
	if c.Publishing {
		props[LifecycleStatus] = obj{"type": "string", "enum": []any{StatusDraft, StatusPublished, StatusArchived}, "readOnly": true}
		props[LifecyclePublishedAt] = readOnlyDateTime()
	}
	if c.SoftDelete {
		props[LifecycleDeletedAt] = readOnlyDateTime()
	}
	return obj{"type": "object", "properties": props}
}

// createInputSchema is the POST body shape: an optional client-supplied id plus
// the writeable fields. required = required fields that have no default.
func (c CollectionDef) createInputSchema() obj {
	props := obj{"id": obj{"type": "string"}}
	var required []any
	for _, f := range c.Fields {
		if f.Type == TypeRelation {
			props[f.Name] = relationInputSchema(f)
		} else {
			props[f.Name] = fieldJSONSchema(f)
		}
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
		if f.Type == TypeRelation {
			props[f.Name] = relationInputSchema(f)
		} else {
			props[f.Name] = fieldJSONSchema(f)
		}
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
