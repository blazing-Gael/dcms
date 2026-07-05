package schema

import "strings"

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

		// Engine-managed collections (e.g. _media) expose their record type — it's
		// referenced by relations — but have no public JSON CRUD routes or input
		// bodies; they're served through dedicated endpoints (see mediaPaths).
		if strings.HasPrefix(c.Name, "_") {
			continue
		}

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
		// Lifecycle transition endpoints (ADR-0012).
		if c.Publishing {
			paths[base+"/"+c.Name+"/{id}/publish"] = obj{"post": transitionOp(c.Name, name, "Publish (optionally schedule via {\"at\": <RFC3339>})")}
			paths[base+"/"+c.Name+"/{id}/unpublish"] = obj{"post": transitionOp(c.Name, name, "Unpublish (return to draft)")}
			paths[base+"/"+c.Name+"/{id}/archive"] = obj{"post": transitionOp(c.Name, name, "Archive (retire, kept but hidden)")}
		}
		if c.SoftDelete {
			paths[base+"/"+c.Name+"/{id}/restore"] = obj{"post": transitionOp(c.Name, name, "Restore a soft-deleted record")}
		}
	}

	// The media library's byte-path endpoints (ADR-0011).
	for p, op := range mediaPaths() {
		paths[p] = op
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
		expandParam(),
		obj{
			"name": "filter", "in": "query", "style": "deepObject", "explode": true,
			"schema":      obj{"type": "object", "additionalProperties": obj{"type": "string"}},
			"description": "field filters, e.g. filter[status]=active or filter[price][gte]=100",
		},
	}
}

// expandParam documents the ?expand= query parameter, which inlines related
// records in the response (belongs-to on lists and single reads; has-many and
// many-to-many on single reads).
func expandParam() obj {
	return queryParam("expand", "string",
		"comma-separated relation fields to inline in the response, e.g. expand=author,tags")
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
		"parameters": []any{idParam(), expandParam()},
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

// transitionOp documents a lifecycle transition endpoint (ADR-0012): a POST that
// flips managed columns server-side and returns the updated record.
func transitionOp(collection, name, summary string) obj {
	return obj{
		"summary":    summary + " — " + collection,
		"parameters": []any{idParam()},
		"responses": obj{
			"200": jsonResponse("the updated "+collection+" record", dataEnvelope(name)),
			"404": jsonResponse("not found", errorEnvelope()),
		},
	}
}

// mediaPaths documents the media library's byte-path endpoints (ADR-0011). They
// live outside the collection API because their write path is multipart bytes.
func mediaPaths() obj {
	m := mediaData()
	notFound := jsonResponse("not found", errorEnvelope())
	upload := obj{"required": true, "content": obj{"multipart/form-data": obj{"schema": obj{
		"type": "object",
		"properties": obj{
			"file":    obj{"type": "string", "format": "binary"},
			"alt":     obj{"type": "string"},
			"title":   obj{"type": "string"},
			"caption": obj{"type": "string"},
		},
		"required": []any{"file"},
	}}}}
	return obj{
		"/__media": obj{
			"get": obj{
				"summary":    "List media assets (the library)",
				"parameters": listParams(),
				"responses":  obj{"200": jsonResponse("a page of media", listEnvelope("Media"))},
			},
			"post": obj{
				"summary":     "Upload a new media asset",
				"requestBody": upload,
				"responses": obj{
					"201": jsonResponse("created", dataEnvelope("Media")),
					"413": jsonResponse("upload too large", errorEnvelope()),
					"415": jsonResponse("unsupported content type", errorEnvelope()),
				},
			},
		},
		"/__media/{id}": obj{
			"get": obj{
				"summary":    "Get a media asset (use ?expand=<collection> for where-used)",
				"parameters": []any{idParam(), expandParam()},
				"responses":  obj{"200": m, "404": notFound},
			},
			"post": obj{
				"summary":     "Replace an asset's file, keeping its id and all references",
				"parameters":  []any{idParam()},
				"requestBody": upload,
				"responses":   obj{"200": m, "404": notFound},
			},
			"patch": obj{
				"summary":    "Edit media metadata (alt/title/caption/filename)",
				"parameters": []any{idParam()},
				"responses":  obj{"200": m, "404": notFound},
			},
			"delete": obj{
				"summary":    "Delete a media asset and its bytes",
				"parameters": []any{idParam()},
				"responses": obj{
					"204": obj{"description": "deleted"},
					"404": notFound,
					"409": jsonResponse("still referenced by records", errorEnvelope()),
				},
			},
		},
		"/__media/{id}/raw": obj{
			"get": obj{
				"summary":    "Stream the asset's bytes (supports HTTP Range)",
				"parameters": []any{idParam()},
				"responses": obj{
					"200": obj{"description": "the file bytes", "content": obj{"application/octet-stream": obj{"schema": obj{"type": "string", "format": "binary"}}}},
					"302": obj{"description": "redirect to the object URL (for S3-backed stores)"},
					"404": notFound,
				},
			},
		},
	}
}

// mediaData is the single-media success response, reused across media ops.
func mediaData() obj { return jsonResponse("the media asset", dataEnvelope("Media")) }

func deleteOp(collection, name string) obj {
	return obj{
		"summary":    "Delete a " + collection + " record",
		"parameters": []any{idParam()},
		"responses": obj{
			"204": obj{"description": "deleted"},
			"404": jsonResponse("not found", errorEnvelope()),
			"409": jsonResponse("still referenced by other records (on_delete: restrict)", errorEnvelope()),
		},
	}
}
