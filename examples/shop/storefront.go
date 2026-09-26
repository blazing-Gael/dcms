package main

import (
	"embed"

	dcms "github.com/blazing-Gael/dcms"
)

//go:embed storefront/index.html storefront/app.js storefront/styles.css
var storefrontFS embed.FS

// serveStorefront mounts the demo storefront (a static single-page app) on the same
// origin as the API — so there's no CORS to configure, and one binary serves the
// store, the admin panel (/__admin), and the API (/api/v1). The files are embedded,
// so `go run .` works from anywhere and a built binary is self-contained.
func serveStorefront(app *dcms.App) {
	file := func(path, ctype string) dcms.RouteFunc {
		body, _ := storefrontFS.ReadFile(path)
		return func(req *dcms.Request) {
			req.W.Header().Set("Content-Type", ctype)
			_, _ = req.W.Write(body)
		}
	}
	app.Route("GET", "/", file("storefront/index.html", "text/html; charset=utf-8"))
	app.Route("GET", "/app.js", file("storefront/app.js", "text/javascript; charset=utf-8"))
	app.Route("GET", "/styles.css", file("storefront/styles.css", "text/css; charset=utf-8"))
}
