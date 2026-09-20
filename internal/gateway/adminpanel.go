package gateway

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// The built-in admin panel (ADR-0035): an embedded, server-rendered window into the
// instance at /__admin, generated from the schema. It is a privileged client, not a
// backdoor — every write goes through the same authorized pipeline as the API, as
// the logged-in principal — and the schema is read-only here.

//go:embed adminassets/templates/*.html adminassets/static/*
var adminAssets embed.FS

const (
	adminBasePath   = "/__admin"
	adminCSRFCookie = "dcms_admin_csrf"
	adminCSRFField  = "csrf"
)

// adminEnabled reports whether the panel should be mounted. Nil options ⇒ on (the
// zero-config default); an explicit Enabled false disables it.
func (s *Server) adminEnabled() bool {
	return s.opts.Admin == nil || s.opts.Admin.Enabled
}

// adminTemplate lazily parses (once) the layout + a page into one template set, so
// New stays cheap for the many servers that never render the panel.
func (s *Server) adminTemplate(page string) *template.Template {
	s.adminTmplOnce.Do(func() {
		s.adminTmpl = map[string]*template.Template{}
		funcs := template.FuncMap{
			"title": func(x string) string {
				return strings.ToUpper(x[:1]) + strings.ReplaceAll(x[1:], "_", " ")
			},
		}
		for _, p := range []string{"overview", "login", "list", "form"} {
			t, err := template.New("").Funcs(funcs).ParseFS(adminAssets,
				"adminassets/templates/layout.html", "adminassets/templates/"+p+".html")
			if err != nil {
				s.logger.Error("admin: template parse failed", "page", p, "err", err)
				continue
			}
			s.adminTmpl[p] = t
		}
	})
	return s.adminTmpl[page]
}

// mountAdmin registers the panel routes. The global withPrincipal middleware has
// already resolved identity, so handlers read the principal from the context.
func (s *Server) mountAdmin(r chi.Router) {
	// Static assets (CSS/JS) — public, no auth needed.
	static, _ := fs.Sub(adminAssets, "adminassets/static")
	r.Handle(adminBasePath+"/static/*", http.StripPrefix(adminBasePath+"/static/",
		http.FileServer(http.FS(static))))

	r.Route(adminBasePath, func(r chi.Router) {
		r.Use(s.limitBody)
		// Login is reachable without a session; everything else requires one.
		r.Get("/login", s.adminLoginForm)
		r.Post("/login", s.adminLogin)
		r.Post("/logout", s.adminLogout)

		r.Group(func(r chi.Router) {
			r.Use(s.adminRequireAuth)
			r.Get("/", s.adminOverview)
			r.Get("/c/{collection}", s.adminList)
			r.Get("/c/{collection}/new", s.adminNewForm)
			r.Post("/c/{collection}", s.adminCreate)
			r.Get("/c/{collection}/{id}", s.adminEditForm)
			r.Post("/c/{collection}/{id}", s.adminUpdate)
			r.Post("/c/{collection}/{id}/delete", s.adminDelete)
		})
	})
}

// adminRequireAuth redirects an unauthenticated visitor to the login page.
func (s *Server) adminRequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principalFromContext(r.Context()).Authenticated {
			http.Redirect(w, r, adminBasePath+"/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── CSRF (double-submit cookie) ───────────────────────────────────────────────
// A cookie-authenticated, form-based UI needs CSRF protection: a per-visitor token
// lives in a cookie and is echoed in every form; a mutating POST must present a
// matching field. An attacker can neither read the cookie nor forge a matching
// field from another origin (SameSite=Lax + HttpOnly).

func (s *Server) adminCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(adminCSRFCookie); err == nil && c.Value != "" {
		return c.Value
	}
	tok, err := newSessionToken()
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCSRFCookie, Value: tok, Path: adminBasePath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.requestIsSecure(r),
	})
	return tok
}

func (s *Server) adminCSRFValid(r *http.Request) bool {
	c, err := r.Cookie(adminCSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.FormValue(adminCSRFField))) == 1
}

// ── rendering ─────────────────────────────────────────────────────────────────

// adminPage is the common template data: the shell (nav, signed-in user, CSRF,
// flash) plus the page-specific Data.
type adminPage struct {
	Title string
	User  string
	Nav   []adminNavItem
	CSRF  string
	Flash string
	Error string
	Data  any
}

type adminNavItem struct{ Name, Label string }

// adminNav lists the collections shown in the sidebar — routable (non-system)
// collections for now (ADR-0035 phase 1).
func (s *Server) adminNav() []adminNavItem {
	var items []adminNavItem
	for _, c := range s.schema.Collections {
		if s.routableCollection(c.Name) {
			items = append(items, adminNavItem{Name: c.Name, Label: c.Name})
		}
	}
	return items
}

// render executes a page within the layout, buffering so a template error never
// emits a half-written page. It fills the shell fields (CSRF cookie, user, nav).
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, page string, data *adminPage) {
	data.CSRF = s.adminCSRF(w, r)
	data.Nav = s.adminNav()
	if p := principalFromContext(r.Context()); p.Authenticated {
		data.User = s.adminUserLabel(r, p.ID)
	}
	t := s.adminTemplate(page)
	if t == nil {
		http.Error(w, "admin template unavailable", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.logger.Error("admin: render failed", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// adminUserLabel returns the signed-in user's email for the header, falling back to
// the id.
func (s *Server) adminUserLabel(r *http.Request, id string) string {
	if rec, err := s.db.FindOne(r.Context(), schema.UsersCollection, id); err == nil {
		if email, _ := rec[schema.UserEmail].(string); email != "" {
			return email
		}
	}
	return id
}
