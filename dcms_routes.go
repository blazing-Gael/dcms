package dcms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Request is the context a custom route handler receives (ADR-0031 §5, ADR-0038).
// It runs after the request's identity is resolved, so Principal is the verified
// caller (its zero value is anonymous).
//
// Store acts as Principal and enforces the same rules as the HTTP API — use it for
// anything done on the caller's behalf. System is the privileged store (no access
// rules), for system work the caller couldn't do directly, such as decrementing
// stock during checkout; unlike a hook, a custom route is not already inside a
// transaction, so open one via System if you need atomic multi-writes.
type Request struct {
	W         http.ResponseWriter
	R         *http.Request
	Principal Principal
	Store     ScopedStore
	System    HookStore

	app *App
}

// Can reports whether the caller may perform action on record id of collection —
// App.Can for Principal, with the request's context.
func (r *Request) Can(action Action, collection, id string) (bool, error) {
	return r.app.Can(r.R.Context(), r.Principal, action, collection, id)
}

// Param returns a path parameter from the route pattern, e.g. "id" for
// "/learn/lessons/{id}".
func (r *Request) Param(name string) string { return chi.URLParam(r.R, name) }

// RouteFunc handles a custom route.
type RouteFunc func(*Request)

// wrapRoute adapts a RouteFunc into an http.HandlerFunc, injecting the verified
// principal (from the middleware-populated context) and the stores.
func (a *App) wrapRoute(fn RouteFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		fn(&Request{W: w, R: r, Principal: p, Store: a.As(p), System: a.srv.DB(), app: a})
	}
}
