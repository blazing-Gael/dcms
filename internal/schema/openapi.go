package schema

import "strings"

// OpenAPI builds an OpenAPI 3.1 document from the schema. Every path, parameter,
// request body, and response is derived from the parsed schema, so the spec can
// never drift from the live API. info.version carries the contract version.
func (s *SchemaDefinition) OpenAPI() obj {
	schemas := obj{"Error": errorSchema(), "RichText": richTextSchema()}
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

		listGet := annotateAccess(listOp(c.Name, name), c.AccessRule(ActionRead))
		createPost := annotateAccess(createOp(c.Name, name), c.AccessRule(ActionCreate))
		getOne := annotateAccess(getOneOp(c.Name, name), c.AccessRule(ActionRead))
		patch := annotateAccess(updateOp(c.Name, name), c.AccessRule(ActionUpdate))
		del := annotateAccess(deleteOp(c.Name, name), c.AccessRule(ActionDelete))

		paths[base+"/"+c.Name] = obj{"get": listGet, "post": createPost}
		paths[base+"/"+c.Name+"/{id}"] = obj{"get": getOne, "patch": patch, "delete": del}
		// Lifecycle transition endpoints (ADR-0012); a transition is a managed write,
		// so it carries the collection's update access (ADR-0016).
		if c.Publishing {
			paths[base+"/"+c.Name+"/{id}/publish"] = obj{"post": annotateAccess(transitionOp(c.Name, name, "Publish (optionally schedule via {\"at\": <RFC3339>})"), c.AccessRule(ActionUpdate))}
			paths[base+"/"+c.Name+"/{id}/unpublish"] = obj{"post": annotateAccess(transitionOp(c.Name, name, "Unpublish (return to draft)"), c.AccessRule(ActionUpdate))}
			paths[base+"/"+c.Name+"/{id}/archive"] = obj{"post": annotateAccess(transitionOp(c.Name, name, "Archive (retire, kept but hidden)"), c.AccessRule(ActionUpdate))}
		}
		if c.SoftDelete {
			paths[base+"/"+c.Name+"/{id}/restore"] = obj{"post": annotateAccess(transitionOp(c.Name, name, "Restore a soft-deleted record"), c.AccessRule(ActionUpdate))}
		}
	}

	// The media library's byte-path endpoints (ADR-0011).
	for p, op := range mediaPaths() {
		paths[p] = op
	}

	// Local authentication endpoints (ADR-0016).
	for p, op := range authPaths() {
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
		"openapi": "3.1.0",
		"info":    info,
		"paths":   paths,
		"components": obj{
			"schemas": schemas,
			"securitySchemes": obj{
				// Opaque session token: `Authorization: Bearer <token>` from
				// POST /auth/login, or the dcms_session cookie (ADR-0016).
				"sessionToken": obj{"type": "http", "scheme": "bearer"},
				"sessionCookie": obj{
					"type": "apiKey", "in": "cookie", "name": "dcms_session",
				},
			},
		},
	}
}

// annotateAccess records a route's authorization rule (ADR-0016) on its operation
// object: a human-readable x-access note plus, for any non-public rule, a security
// requirement so generated clients and docs know a session token is needed.
func annotateAccess(op obj, rule Rule) obj {
	op["x-access"] = accessNote(rule)
	if rule.Kind != RulePublic {
		op["security"] = []obj{{"sessionToken": []string{}}, {"sessionCookie": []string{}}}
	}
	return op
}

// accessNote renders an access rule as a short human-readable string.
func accessNote(rule Rule) string {
	switch rule.Kind {
	case RuleRoles:
		return "roles: " + strings.Join(rule.Roles, ", ")
	case RuleAny:
		parts := make([]string, len(rule.Any))
		for i, sub := range rule.Any {
			parts[i] = accessNote(sub)
		}
		return "any(" + strings.Join(parts, " | ") + ")"
	default:
		return string(rule.Kind)
	}
}

// authPaths returns the OpenAPI paths for the local auth endpoints (ADR-0016).
func authPaths() obj {
	credentials := obj{
		"type":     "object",
		"required": []string{"email", "password"},
		"properties": obj{
			"email":    obj{"type": "string", "format": "email"},
			"password": obj{"type": "string", "format": "password"},
		},
	}
	return obj{
		"/auth/login": obj{"post": obj{
			"summary":     "Log in with email + password; returns an opaque session token",
			"tags":        []string{"auth"},
			"requestBody": obj{"required": true, "content": obj{"application/json": obj{"schema": credentials}}},
			"responses": obj{
				"200": obj{"description": "authenticated; token in body and Set-Cookie"},
				"401": obj{"description": "invalid email or password"},
			},
		}},
		"/auth/logout": obj{"post": obj{
			"summary":   "Revoke the current session",
			"tags":      []string{"auth"},
			"responses": obj{"204": obj{"description": "logged out (idempotent)"}},
		}},
		"/auth/me": obj{"get": obj{
			"summary":   "Return the current principal (id, roles, email)",
			"tags":      []string{"auth"},
			"security":  []obj{{"sessionToken": []string{}}, {"sessionCookie": []string{}}},
			"responses": obj{"200": obj{"description": "the authenticated principal"}, "401": obj{"description": "not authenticated"}},
		}},
	}
}

// ── building blocks ──────────────────────────────────────────────────────────

func ref(name string) obj { return obj{"$ref": "#/components/schemas/" + name} }

// richTextSchema is the reusable component for structured rich content (ADR-0014):
// an array of nodes — text blocks with spans and markDefs, or custom blocks
// (image/reference/code/embed) discriminated by _type. Custom blocks stay open
// (additionalProperties) because their shape varies by _type.
func richTextSchema() obj {
	span := obj{
		"type": "object",
		"properties": obj{
			"_type": obj{"const": "span"},
			"text":  obj{"type": "string"},
			"marks": obj{"type": "array", "items": obj{"type": "string"}},
		},
		"required": []any{"_type", "text"},
	}
	markDef := obj{
		"type": "object",
		"properties": obj{
			"_key":  obj{"type": "string"},
			"_type": obj{"type": "string"},
		},
		"required":             []any{"_key", "_type"},
		"additionalProperties": true,
	}
	block := obj{
		"type": "object",
		"properties": obj{
			"_type":    obj{"const": "block"},
			"style":    obj{"type": "string"},
			"listItem": obj{"type": "string", "enum": []any{"bullet", "number"}},
			"level":    obj{"type": "integer"},
			"children": obj{"type": "array", "items": span},
			"markDefs": obj{"type": "array", "items": markDef},
		},
		"required": []any{"_type"},
	}
	custom := obj{
		"type":                 "object",
		"properties":           obj{"_type": obj{"type": "string"}},
		"required":             []any{"_type"},
		"additionalProperties": true,
		"description":          "a custom block (image/reference/code/embed), discriminated by _type",
	}
	return obj{
		"type":        "array",
		"description": "structured rich content (portable-text-style)",
		"items":       obj{"anyOf": []any{block, custom}},
	}
}

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

// includedSchema documents the optional `included` reference manifest (ADR-0015):
// entities resolved by ?expand= on a richtext field, keyed "<collection>:<id>".
func includedSchema() obj {
	return obj{
		"type":                 "object",
		"additionalProperties": obj{"type": "object"},
		"description":          "reference manifest: entities resolved by ?expand= on a richtext field, keyed \"<collection>:<id>\"",
	}
}

func dataEnvelope(recordName string) obj {
	return obj{"type": "object", "properties": obj{
		"data":     ref(recordName),
		"meta":     obj{"type": "object"},
		"included": includedSchema(),
	}}
}

func listEnvelope(recordName string) obj {
	return obj{"type": "object", "properties": obj{
		"data": obj{"type": "array", "items": ref(recordName)},
		"meta": obj{"type": "object", "properties": obj{
			"total":       obj{"type": "integer", "description": "omitted when ?count=false"},
			"limit":       obj{"type": "integer"},
			"next_cursor": obj{"type": "string"},
		}},
		"included": includedSchema(),
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
		queryParam("count", "boolean", "set false to skip the total row count (cheaper pages; meta.total is then omitted)"),
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
		"comma-separated fields to inline in the response: relation fields (e.g. expand=author,tags) "+
			"and richtext fields, which resolve their in-content references under a _resolved key (e.g. expand=body)")
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
		"parameters":  []any{idempotencyKeyParam()},
		"requestBody": reqBody(name + "CreateInput"),
		"responses": obj{
			"201": jsonResponse("created", dataEnvelope(name)),
			"409": jsonResponse("conflict (unique constraint, or an in-flight request with the same Idempotency-Key)", errorEnvelope()),
			"422": jsonResponse("validation error (or Idempotency-Key reused with a different body)", errorEnvelope()),
		},
	}
}

// idempotencyKeyParam documents the optional Idempotency-Key request header
// (ADR-0018): a client-generated key that makes a retried create safe — the
// original response is replayed instead of creating a duplicate.
func idempotencyKeyParam() obj {
	return obj{
		"name": "Idempotency-Key", "in": "header", "required": false,
		"schema":      obj{"type": "string", "maxLength": 255},
		"description": "Optional client-generated key (e.g. a UUID) that makes this create idempotent: a retry with the same key replays the original response instead of creating a duplicate. Reusing a key with a different body is a 422.",
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
