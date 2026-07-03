// Package codegen turns a parsed schema into client code. It is one more
// consumer of the schema IR (see ADR-0008): the types it emits derive from the
// same field definitions the runtime validator and OpenAPI spec use, so a
// generated SDK cannot drift from the server that produced it.
//
// Structure: BuildModel (model.go) produces a language-neutral Model; each
// Backend maps that Model to one language's source. Adding an SDK is a new
// Backend, not a new walk of the schema.
package codegen

import (
	"fmt"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// Backend renders the neutral Model as one language's client module.
type Backend interface {
	// Filename is the file the generated source should be written to.
	Filename() string
	// Generate renders the module source.
	Generate(Model) (string, error)
}

// backendFor returns the Backend for a language token, or false if unsupported.
func backendFor(lang string) (Backend, bool) {
	switch lang {
	case "ts", "typescript":
		return tsBackend{}, true
	default:
		return nil, false
	}
}

// Generate produces client source for lang from a parsed schema, returning the
// suggested filename and the rendered content.
func Generate(def *schema.SchemaDefinition, lang string) (filename, content string, err error) {
	b, ok := backendFor(lang)
	if !ok {
		return "", "", fmt.Errorf("unsupported language %q (supported: ts)", lang)
	}
	content, err = b.Generate(BuildModel(def))
	if err != nil {
		return "", "", err
	}
	return b.Filename(), content, nil
}
