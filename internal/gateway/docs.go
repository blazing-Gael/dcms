package gateway

import (
	"fmt"
	"html"
	"net/http"
)

// docsHTML renders interactive API docs with Scalar, pointed at the live
// /__openapi spec — so the docs always match the running contract. The %s is the
// page title.
//
// The viewer script is loaded from a CDN. TODO(later): make the viewer source
// configurable / embeddable for fully offline (sovereign) deployments.
const docsHTML = `<!doctype html>
<html>
  <head>
    <title>%s</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/__openapi"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>`

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	title := s.schema.Meta.Name
	if title == "" {
		title = "DCMS API"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, docsHTML, html.EscapeString(title))
}
