// Package gateway builds the virtual HTTP router from a parsed schema.
//
// For every collection it registers CRUD routes (list/create/get/update/delete)
// plus the introspection endpoints (/__schema, /__health, /__ready). Authorization
// (RBAC) is enforced here at the gateway layer in Phase 2 — never inside the store.
//
// See DEV_ROADMAP.md section 1.3 for the Phase 1 router acceptance criteria.
package gateway

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store"
)

const defaultBaseURL = "/api/v1"

// engineColTypes are the columns the engine adds to every collection; they are
// valid for filtering/sorting even though they aren't declared in the schema.
var engineColTypes = map[string]schema.FieldType{
	"id":         schema.TypeString,
	"created_at": schema.TypeDateTime,
	"updated_at": schema.TypeDateTime,
	"created_by": schema.TypeString,
	"updated_by": schema.TypeString,
}

// Server wires a parsed schema and a storage adapter into an http.Handler.
type Server struct {
	schema      *schema.SchemaDefinition
	db          store.Adapter
	collections map[string]schema.CollectionDef // by name, for O(1) lookup
	logger      *slog.Logger
}

// New constructs a gateway Server. If logger is nil, slog.Default() is used.
func New(s *schema.SchemaDefinition, db store.Adapter, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	cols := make(map[string]schema.CollectionDef, len(s.Collections))
	for _, c := range s.Collections {
		cols[c.Name] = c
	}
	return &Server{schema: s, db: db, collections: cols, logger: logger}
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(s.requestLogger)
	r.Use(s.recoverer)

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(s.handleMethodNotAllowed)

	// Introspection / probes.
	r.Get("/__health", s.handleHealth)
	r.Get("/__ready", s.handleReady)
	r.Get("/__schema", s.handleSchema)
	// TODO(phase-1.4): /__openapi — generated OpenAPI 3.1 spec.

	// Virtual collection routes under the configured base URL.
	r.Route(s.baseURL(), func(r chi.Router) {
		r.Get("/{collection}", s.handleList)
		r.Post("/{collection}", s.handleCreate)
		r.Get("/{collection}/{id}", s.handleGetOne)
		r.Patch("/{collection}/{id}", s.handleUpdate)
		r.Delete("/{collection}/{id}", s.handleDelete)
	})

	return r
}

// baseURL returns the API base path: the schema's meta.base_url, sanitized, or
// the default. chi requires a clean prefix with a leading and no trailing slash.
func (s *Server) baseURL() string {
	b := strings.TrimSpace(s.schema.Meta.BaseURL)
	if b == "" {
		return defaultBaseURL
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return strings.TrimRight(b, "/")
}

// knownCollection reports whether name is a collection declared in the schema.
func (s *Server) knownCollection(name string) bool {
	_, ok := s.collections[name]
	return ok
}

// fieldType returns the schema type of a column (declared field or engine column)
// and whether it exists at all on the collection.
func (s *Server) fieldType(collection, field string) (schema.FieldType, bool) {
	if t, ok := engineColTypes[field]; ok {
		return t, true
	}
	cd, ok := s.collections[collection]
	if !ok {
		return "", false
	}
	for _, f := range cd.Fields {
		if f.Name == field {
			return f.Type, true
		}
	}
	return "", false
}

func (s *Server) hasColumn(collection, field string) bool {
	_, ok := s.fieldType(collection, field)
	return ok
}
