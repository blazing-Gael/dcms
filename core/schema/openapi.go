package schema

// OpenAPI builds an OpenAPI 3.1 document from the schema. Every path, parameter,
// request body, and response is derived from the parsed schema, so the spec can
// never drift from the live API. info.version carries the contract version.
func (s *SchemaDefinition) OpenAPI() obj {
	schemas := obj{"Error": errorSchema()}
	paths := obj{}
	base := s.BaseURL()

	for _, c := range s.Collections {
		name := pascal(c.Name)
		schemas[name] = c.recordSchema()
		schemas[name+"CreateInput"] = c.createInputSchema()
		schemas[name+"UpdateInput"] = c.updateInputSchema()

		paths[base+"/"+c.Name] = obj{
			"get":  listOp(c.Name, name),
			"post": createOp(c.Name, name),
		}
		paths[base+"/"+c.Name+"/{id}"] = obj{
			"get":    getOneOp(c.Name, name),
			"patch":  updateOp(c.Name, name),
			"delete": deleteOp(c.Name, name),
		}
	}

	title := s.Meta.Name
	if title == "" {
		title = "DCMS API"
	}
	info := obj{"title": title, "version": s.ContractVersion()}
	if s.Meta.Description != "" {
		info["description"] = s.Meta.Description
	}

	return obj{
		"openapi":    "3.1.0",
		"info":       info,
		"paths":      paths,
		"components": obj{"schemas": schemas},
	}
}

// ── building blocks ──────────────────────────────────────────────────────────

func ref(name string) obj { return obj{"$ref": "#/components/schemas/" + name} }

func errorSchema() obj {
	return obj{
		"type": "object",
		"properties": obj{
			"code":    obj{"type": "string"},
			"message": obj{"type": "string"},
			"fields":  obj{"type": "object", "additionalProperties": obj{"type": "string"}},
		},
		"required": []any{"code", "message"},
	}
}

func dataEnvelope(recordName string) obj {
	return obj{"type": "object", "properties": obj{
		"data": ref(recordName),
		"meta": obj{"type": "object"},
	}}
}

func listEnvelope(recordName string) obj {
	return obj{"type": "object", "properties": obj{
		"data": obj{"type": "array", "items": ref(recordName)},
		"meta": obj{"type": "object", "properties": obj{
			"total":       obj{"type": "integer"},
			"limit":       obj{"type": "integer"},
			"next_cursor": obj{"type": "string"},
		}},
	}}
}

func errorEnvelope() obj {
	return obj{"type": "object", "properties": obj{"error": ref("Error")}}
}

func jsonResponse(desc string, schema obj) obj {
	return obj{"description": desc, "content": obj{"application/json": obj{"schema": schema}}}
}

func queryParam(name, typ, desc string) obj {
	return obj{"name": name, "in": "query", "required": false, "schema": obj{"type": typ}, "description": desc}
}

func idParam() obj {
	return obj{"name": "id", "in": "path", "required": true, "schema": obj{"type": "string"}}
}

func listParams() []any {
	return []any{
		queryParam("limit", "integer", "page size (default 20, max 100)"),
		queryParam("cursor", "string", "keyset pagination cursor from a previous response"),
		queryParam("sort", "string", "field to sort by; prefix with - for descending"),
		queryParam("fields", "string", "comma-separated sparse fieldset"),
		obj{
			"name": "filter", "in": "query", "style": "deepObject", "explode": true,
			"schema":      obj{"type": "object", "additionalProperties": obj{"type": "string"}},
			"description": "field filters, e.g. filter[status]=active or filter[price][gte]=100",
		},
	}
}

func reqBody(ref string) obj {
	return obj{"required": true, "content": obj{"application/json": obj{"schema": obj{"$ref": "#/components/schemas/" + ref}}}}
}

func listOp(collection, name string) obj {
	return obj{
		"summary":    "List " + collection,
		"parameters": listParams(),
		"responses":  obj{"200": jsonResponse("a page of "+collection, listEnvelope(name))},
	}
}

func createOp(collection, name string) obj {
	return obj{
		"summary":     "Create a " + collection + " record",
		"requestBody": reqBody(name + "CreateInput"),
		"responses": obj{
			"201": jsonResponse("created", dataEnvelope(name)),
			"409": jsonResponse("conflict (unique constraint)", errorEnvelope()),
			"422": jsonResponse("validation error", errorEnvelope()),
		},
	}
}

func getOneOp(collection, name string) obj {
	return obj{
		"summary":    "Get a " + collection + " record by id",
		"parameters": []any{idParam()},
		"responses": obj{
			"200": jsonResponse("the "+collection+" record", dataEnvelope(name)),
			"404": jsonResponse("not found", errorEnvelope()),
		},
	}
}

func updateOp(collection, name string) obj {
	return obj{
		"summary":     "Update a " + collection + " record",
		"parameters":  []any{idParam()},
		"requestBody": reqBody(name + "UpdateInput"),
		"responses": obj{
			"200": jsonResponse("updated", dataEnvelope(name)),
			"404": jsonResponse("not found", errorEnvelope()),
			"409": jsonResponse("conflict (unique constraint)", errorEnvelope()),
			"422": jsonResponse("validation error", errorEnvelope()),
		},
	}
}

func deleteOp(collection, name string) obj {
	return obj{
		"summary":    "Delete a " + collection + " record",
		"parameters": []any{idParam()},
		"responses": obj{
			"204": obj{"description": "deleted"},
			"404": jsonResponse("not found", errorEnvelope()),
		},
	}
}
