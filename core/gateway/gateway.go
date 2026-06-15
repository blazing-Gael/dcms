// Package gateway builds the virtual HTTP router from a parsed schema.
//
// For every collection it registers CRUD routes (list/create/get/update/delete)
// plus the introspection endpoints (/__schema, /__health, /__ready, /__openapi).
// Authorization (RBAC) is enforced here at the gateway layer in Phase 2 — never
// inside collection handlers.
//
// See DEV_ROADMAP.md section 1.3 for the Phase 1 router acceptance criteria.
package gateway

import (
	"net/http"

	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store"
)

// Server wires a parsed schema and a storage adapter into an http.Handler.
type Server struct {
	schema *schema.SchemaDefinition
	db     store.Adapter
}

// New constructs a gateway Server.
func New(s *schema.SchemaDefinition, db store.Adapter) *Server {
	return &Server{schema: s, db: db}
}

// Handler returns the root http.Handler with all collection and introspection
// routes registered.
//
// TODO(phase-1): build a chi router, register CRUD + __ routes per collection.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/__health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}
