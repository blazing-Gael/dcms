package schema

import (
	"encoding/json"
	"testing"
)

const openapiSchema = `
version: "1"
meta:
  name: shop
  base_url: /api/v1
collections:
  products:
    fields:
      title:
        type: string
        required: true
      price:
        type: number
        required: true
      stock:
        type: integer
        default: 0
      status:
        type: enum
        values: [draft, active]
        default: draft
`

func mustParse(t *testing.T, src string) *SchemaDefinition {
	t.Helper()
	def, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return def
}

func TestContractHash_StableAndSensitive(t *testing.T) {
	a := mustParse(t, openapiSchema)
	b := mustParse(t, openapiSchema)
	if a.ContractHash() != b.ContractHash() {
		t.Fatal("hash should be stable for identical schemas")
	}

	changed := mustParse(t, openapiSchema+`
      extra:
        type: string`)
	if a.ContractHash() == changed.ContractHash() {
		t.Fatal("hash should change when the schema changes")
	}
}

func TestOpenAPI_StructureAndDerivation(t *testing.T) {
	def := mustParse(t, openapiSchema)
	doc := def.OpenAPI()

	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi version: %v", doc["openapi"])
	}
	info := doc["info"].(obj)
	if info["version"] != def.ContractVersion() {
		t.Fatalf("info.version should be the contract version: %v", info["version"])
	}

	paths := doc["paths"].(obj)
	if _, ok := paths["/api/v1/products"]; !ok {
		t.Fatalf("missing collection path: %v", keys(paths))
	}
	if _, ok := paths["/api/v1/products/{id}"]; !ok {
		t.Fatalf("missing item path: %v", keys(paths))
	}

	schemas := doc["components"].(obj)["schemas"].(obj)
	for _, want := range []string{"Products", "ProductsCreateInput", "ProductsUpdateInput", "Error"} {
		if _, ok := schemas[want]; !ok {
			t.Fatalf("missing component schema %q: %v", want, keys(schemas))
		}
	}

	// createInput.required must contain title+price (required, no default) but not
	// stock/status (defaulted) nor id.
	create := schemas["ProductsCreateInput"].(obj)
	req := toStringSet(create["required"])
	if !req["title"] || !req["price"] {
		t.Fatalf("createInput required should include title+price: %v", create["required"])
	}
	if req["stock"] || req["status"] || req["id"] {
		t.Fatalf("createInput required should exclude defaulted/id fields: %v", create["required"])
	}

	// enum surfaces in the field schema.
	statusSchema := create["properties"].(obj)["status"].(obj)
	if _, ok := statusSchema["enum"]; !ok {
		t.Fatalf("status field should carry an enum: %v", statusSchema)
	}

	// record schema marks id readOnly.
	record := schemas["Products"].(obj)
	idSchema := record["properties"].(obj)["id"].(obj)
	if idSchema["readOnly"] != true {
		t.Fatalf("id should be readOnly in the record schema: %v", idSchema)
	}

	// The whole document must marshal to valid JSON.
	if _, err := json.Marshal(doc); err != nil {
		t.Fatalf("OpenAPI doc does not marshal: %v", err)
	}
}

func keys(m obj) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func toStringSet(v any) map[string]bool {
	set := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				set[s] = true
			}
		}
	}
	return set
}
