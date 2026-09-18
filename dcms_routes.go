package dcms

import (
	"net/http"

	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Request is the context a custom route handler receives (ADR-0031 §5). It runs
// after the request's identity is resolved, so Principal is the verified caller
// (its zero value is anonymous). Store is the app's store handle, so a handler can
// read and write like a hook — though, unlike a hook, a custom route is not already
// inside a transaction; open one via Store if you need atomic multi-writes.
type Request struct {
	W         http.ResponseWriter
	R         *http.Request
	Principal Principal
	Store     HookStore
}

// RouteFunc handles a custom route.
type RouteFunc func(*Request)

// wrapRoute adapts a RouteFunc into an http.HandlerFunc, injecting the verified
// principal (from the middleware-populated context) and the app's store.
func (a *App) wrapRoute(fn RouteFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(&Request{W: w, R: r, Principal: auth.FromContext(r.Context()), Store: a.db})
	}
}
