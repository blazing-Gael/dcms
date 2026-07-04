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

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// mediaBasePath is where the media library's byte-path endpoints live. It is
// outside the collection API (/api/v1) because media's write path is bytes, not
// JSON (ADR-0011).
const mediaBasePath = "/__media"

// defaultMaxUploadBytes caps a single upload when none is configured (32 MiB).
const defaultMaxUploadBytes = 32 << 20

// engineColTypes are the columns the engine adds to every collection; they are
// valid for filtering/sorting even though they aren't declared in the schema.
var engineColTypes = map[string]schema.FieldType{
	"id":         schema.TypeString,
	"created_at": schema.TypeDateTime,
	"updated_at": schema.TypeDateTime,
	"created_by": schema.TypeString,
	"updated_by": schema.TypeString,
}

// Options tunes gateway behavior. The zero value is a valid, permissive config.
type Options struct {
	// ValidateResponses turns on strict response validation: every outgoing
	// record is checked against the schema, and a violation returns 500 instead
	// of shipping non-conforming data. It guards against store/adapter drift and
	// is best enabled in dev/CI (see ADR-0008). Off by default so production
	// pays nothing and never fails a read over a stored-data quirk.
	ValidateResponses bool

	// Blob is the byte store backing the media library (ADR-0011). When nil, the
	// /__media endpoints report 503 — media is unconfigured.
	Blob blob.Store
	// MaxUploadBytes caps a single media upload; 0 uses defaultMaxUploadBytes.
	MaxUploadBytes int64
	// AllowedContentTypes optionally restricts uploads (exact matches, or a
	// trailing-slash prefix like "image/"). Empty means any type is accepted.
	AllowedContentTypes []string
}

// Server wires a parsed schema and a storage adapter into an http.Handler.
type Server struct {
	schema      *schema.SchemaDefinition
	db          store.Adapter
	collections map[string]schema.CollectionDef // by name, for O(1) lookup
	logger      *slog.Logger
	opts        Options
}

// New constructs a gateway Server. If logger is nil, slog.Default() is used.
// Options are optional; the zero value applies when none is passed.
func New(s *schema.SchemaDefinition, db store.Adapter, logger *slog.Logger, opts ...Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	cols := make(map[string]schema.CollectionDef, len(s.Collections))
	for _, c := range s.Collections {
		cols[c.Name] = c
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return &Server{schema: s, db: db, collections: cols, logger: logger, opts: o}
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
	r.Get("/__openapi", s.handleOpenAPI)
	r.Get("/__docs", s.handleDocs)

	// Media library (byte path) — outside the collection API (ADR-0011).
	r.Route(mediaBasePath, func(r chi.Router) {
		r.Get("/", s.handleMediaList)
		r.Post("/", s.handleMediaUpload)
		r.Get("/{id}", s.handleMediaGet)
		r.Post("/{id}", s.handleMediaReplace)
		r.Patch("/{id}", s.handleMediaPatch)
		r.Delete("/{id}", s.handleMediaDelete)
		r.Get("/{id}/raw", s.handleMediaRaw)
	})

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

// baseURL returns the API base path. Delegates to the schema so the router and
// the generated OpenAPI spec always agree on the prefix.
func (s *Server) baseURL() string {
	return s.schema.BaseURL()
}

// knownCollection reports whether name is a collection declared in the schema.
func (s *Server) knownCollection(name string) bool {
	_, ok := s.collections[name]
	return ok
}

// routableCollection reports whether a name is reachable through the public
// collection API. Engine-managed collections (a leading underscore, e.g. _media)
// exist for expansion, migration, and typing but are not JSON-CRUD routable —
// _media is served through its own byte-path endpoints instead (ADR-0011).
func (s *Server) routableCollection(name string) bool {
	return s.knownCollection(name) && !strings.HasPrefix(name, "_")
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
