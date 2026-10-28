package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Parity tests for the embedding API (ADR-0038): Can and As(p) must give exactly the
// answers the HTTP API gives the same caller, for every rule kind.

const embedSchema = `
version: "1"
auth:
  roles:
    admin:   { label: Administrator }
    teacher: { label: Teacher }
  session: { ttl: 1h }
collections:
  courses:
    fields: { title: { type: string } }
    access: { read: public, create: [admin] }
  enrollments:
    fields:
      user:   { type: relation, target: _users, required: true }
      course: { type: relation, target: courses, required: true }
    access: { read: [admin], create: [admin] }
  lessons:
    publishing: true
    revisions: true
    fields:
      course: { type: relation, target: courses, required: true }
      title:  { type: string }
      answer:
        type: string
        access: { read: [admin, teacher] }
    access:
      read:
        any:
          - admin
          - teacher
          - exists: { in: enrollments, match: { course: course }, principal: user }
      create:  [admin]
      update:  [admin, teacher]
      delete:  [admin]
      publish: [admin]
  notes:
    soft_delete: true
    fields: { body: { type: string } }
    access:
      read:   owner
      create: authenticated
      update: owner
      delete: owner
`

// embedServer builds one gateway and serves it over HTTP, so a test can ask the same
// instance in-process and over the wire.
func embedServer(t *testing.T, src string, opts gateway.Options) (*gateway.Server, string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// A file database, not :memory: — each pooled connection to :memory: is a separate
	// empty database, which would hide what a second connection (e.g. a read from
	// inside a hook's transaction) really sees.
	db, err := sqlite.New(sqlite.Config{Path: t.TempDir() + "/embed.db"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	opts.Authenticator = gateway.NewSessionAuthenticator(db)
	gw := gateway.New(def, db, nil, opts)
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return gw, srv.URL, db
}

// caller is one identity, both as a bearer token (HTTP) and a Principal (Go).
type caller struct {
	name  string
	token string
	p     auth.Principal
}

func newCaller(t *testing.T, base string, db store.Adapter, name string, roles ...string) caller {
	t.Helper()
	if name == "anonymous" {
		return caller{name: name}
	}
	email, pw := name+"@x.com", "pw-"+name+"-12345"
	id := seedUserID(t, db, email, pw, roles...)
	return caller{name: name, token: login(t, base, email, pw), p: auth.Principal{ID: id, Roles: roles, Authenticated: true}}
}

type embedFixture struct {
	gw                      *gateway.Server
	base                    string
	db                      store.Adapter
	admin, teacher, student caller
	outsider, anon          caller
	c1, c2                  string
	l1, l2, draft           string // l1 in c1 (published), l2 in c2 (published), draft in c1
	note                    string // student's note
}

func newEmbedFixture(t *testing.T) embedFixture {
	t.Helper()
	gw, base, db := embedServer(t, embedSchema, gateway.Options{})
	f := embedFixture{gw: gw, base: base, db: db}
	f.admin = newCaller(t, base, db, "admin", "admin")
	f.teacher = newCaller(t, base, db, "teacher", "teacher")
	f.student = newCaller(t, base, db, "student")
	f.outsider = newCaller(t, base, db, "outsider")
	f.anon = newCaller(t, base, db, "anonymous")

	mk := func(tok, path, body string) string {
		st, b := doAs(t, http.MethodPost, base+path, tok, body)
		if st != http.StatusCreated {
			t.Fatalf("seed POST %s: %d %v", path, st, b)
		}
		return recordID(t, b)
	}
	f.c1 = mk(f.admin.token, "/api/v1/courses", `{"title":"c1"}`)
	f.c2 = mk(f.admin.token, "/api/v1/courses", `{"title":"c2"}`)
	mk(f.admin.token, "/api/v1/enrollments", `{"user":"`+f.student.p.ID+`","course":"`+f.c1+`"}`)
	f.l1 = mk(f.admin.token, "/api/v1/lessons", `{"course":"`+f.c1+`","title":"one","answer":"42"}`)
	f.l2 = mk(f.admin.token, "/api/v1/lessons", `{"course":"`+f.c2+`","title":"two","answer":"7"}`)
	f.draft = mk(f.admin.token, "/api/v1/lessons", `{"course":"`+f.c1+`","title":"draft","answer":"x"}`)
	for _, id := range []string{f.l1, f.l2} {
		if st, _ := doAs(t, http.MethodPost, base+"/api/v1/lessons/"+id+"/publish", f.admin.token, ""); st != http.StatusOK {
			t.Fatalf("publish %s: %d", id, st)
		}
	}
	f.note = mk(f.student.token, "/api/v1/notes", `{"body":"mine"}`)
	return f
}

func (f embedFixture) callers() []caller {
	return []caller{f.admin, f.teacher, f.student, f.outsider, f.anon}
}

func allowed(status int) bool { return status >= 200 && status < 300 }

// Can(read/update/history) agrees with the HTTP API for every caller and record.
func TestEmbed_CanMatchesHTTP(t *testing.T) {
	f := newEmbedFixture(t)
	ctx := context.Background()
	records := []struct{ coll, id string }{
		{"lessons", f.l1}, {"lessons", f.l2}, {"lessons", f.draft}, {"lessons", "missing"},
		{"notes", f.note}, {"notes", "missing"}, {"courses", f.c1},
	}
	for _, c := range f.callers() {
		for _, r := range records {
			url := f.base + "/api/v1/" + r.coll + "/" + r.id

			st, _ := doAs(t, http.MethodGet, url, c.token, "")
			if got, err := f.gw.Can(ctx, c.p, schema.ActionRead, r.coll, r.id); err != nil || got != allowed(st) {
				t.Errorf("%s read %s/%s: Can=%v err=%v, HTTP %d", c.name, r.coll, r.id, got, err, st)
			}

			if r.coll == "lessons" {
				st, _ := doAs(t, http.MethodGet, url+"/revisions", c.token, "")
				if got, err := f.gw.Can(ctx, c.p, schema.ActionHistory, r.coll, r.id); err != nil || got != allowed(st) {
					t.Errorf("%s history %s/%s: Can=%v err=%v, HTTP %d", c.name, r.coll, r.id, got, err, st)
				}
			}

			// Ask Can first: the PATCH below writes (an idempotent value, but still).
			can, err := f.gw.Can(ctx, c.p, schema.ActionUpdate, r.coll, r.id)
			body := `{"title":"t"}`
			if r.coll == "notes" {
				body = `{"body":"mine"}`
			}
			st, _ = doAs(t, http.MethodPatch, url, c.token, body)
			if err != nil || can != allowed(st) {
				t.Errorf("%s update %s/%s: Can=%v err=%v, HTTP %d", c.name, r.coll, r.id, can, err, st)
			}
		}

		// create (id ignored)
		for _, coll := range []string{"lessons", "notes", "courses"} {
			can, err := f.gw.Can(ctx, c.p, schema.ActionCreate, coll, "")
			body := `{"course":"` + f.c1 + `","title":"new"}`
			if coll == "notes" {
				body = `{"body":"new"}`
			} else if coll == "courses" {
				body = `{"title":"new"}`
			}
			st, _ := doAs(t, http.MethodPost, f.base+"/api/v1/"+coll, c.token, body)
			if err != nil || can != allowed(st) {
				t.Errorf("%s create %s: Can=%v err=%v, HTTP %d", c.name, coll, can, err, st)
			}
		}
	}
}

// Delete and publish change state, so each caller gets fresh records.
func TestEmbed_CanDestructiveMatchesHTTP(t *testing.T) {
	f := newEmbedFixture(t)
	ctx := context.Background()
	for _, c := range f.callers() {
		_, b := doAs(t, http.MethodPost, f.base+"/api/v1/notes", f.student.token, `{"body":"x"}`)
		note := recordID(t, b)
		can, err := f.gw.Can(ctx, c.p, schema.ActionDelete, "notes", note)
		st, _ := doAs(t, http.MethodDelete, f.base+"/api/v1/notes/"+note, c.token, "")
		if err != nil || can != allowed(st) {
			t.Errorf("%s delete note: Can=%v err=%v, HTTP %d", c.name, can, err, st)
		}

		_, b = doAs(t, http.MethodPost, f.base+"/api/v1/lessons", f.admin.token, `{"course":"`+f.c1+`","title":"d"}`)
		draft := recordID(t, b)
		can, err = f.gw.Can(ctx, c.p, schema.ActionPublish, "lessons", draft)
		st, _ = doAs(t, http.MethodPost, f.base+"/api/v1/lessons/"+draft+"/publish", c.token, "")
		if err != nil || can != allowed(st) {
			t.Errorf("%s publish lesson: Can=%v err=%v, HTTP %d", c.name, can, err, st)
		}
	}
}

// As(p).Get and List return what the HTTP API returns: same rows, same masking.
func TestEmbed_ScopedReadsMatchHTTP(t *testing.T) {
	f := newEmbedFixture(t)
	ctx := context.Background()
	for _, c := range f.callers() {
		s := f.gw.As(c.p)

		st, body := doAs(t, http.MethodGet, f.base+"/api/v1/lessons/"+f.l1, c.token, "")
		rec, err := s.Get(ctx, "lessons", f.l1)
		if allowed(st) != (err == nil) {
			t.Fatalf("%s Get l1: err=%v, HTTP %d", c.name, err, st)
		}
		if err == nil {
			_, httpHas := dataField(t, body)["answer"]
			_, goHas := rec["answer"]
			if httpHas != goHas {
				t.Errorf("%s Get l1 masking: Go has answer=%v, HTTP has answer=%v", c.name, goHas, httpHas)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s Get l1: want ErrNotFound, got %v", c.name, err)
		}

		st, body = doAs(t, http.MethodGet, f.base+"/api/v1/lessons", c.token, "")
		page, err := s.List(ctx, store.Query{Collection: "lessons"})
		if allowed(st) != (err == nil) {
			t.Fatalf("%s List: err=%v, HTTP %d", c.name, err, st)
		}
		if err == nil {
			httpRows, _ := body["data"].([]any)
			if len(page.Data) != len(httpRows) {
				t.Errorf("%s List: Go %d rows, HTTP %d", c.name, len(page.Data), len(httpRows))
			}
		}
	}
}

// Filtering on a field the caller can't read is "unknown field" in Go as over HTTP;
// subquery filters are refused; an owner-scoped filter can't see others' rows.
func TestEmbed_ScopedListGuards(t *testing.T) {
	f := newEmbedFixture(t)
	ctx := context.Background()
	answer := store.Query{Collection: "lessons", Filters: []store.Filter{{Field: "answer", Operator: store.Eq, Value: "42"}}}

	st, _ := doAs(t, http.MethodGet, f.base+"/api/v1/lessons?filter[answer]=42", f.student.token, "")
	_, err := f.gw.As(f.student.p).List(ctx, answer)
	var ve *store.ValidationError
	if st != http.StatusUnprocessableEntity || !errors.As(err, &ve) {
		t.Errorf("masked filter: HTTP %d, Go err %v; want 422 and a ValidationError", st, err)
	}
	if page, err := f.gw.As(f.teacher.p).List(ctx, answer); err != nil || len(page.Data) != 1 {
		t.Errorf("teacher may filter on answer: %d rows, err %v", len(page.Data), err)
	}
	if _, err := f.gw.As(f.student.p).List(ctx, store.Query{Collection: "lessons", Sort: "-answer"}); !errors.As(err, &ve) {
		t.Errorf("sort on masked field: want ValidationError, got %v", err)
	}
	sub := store.Query{Collection: "lessons", Filters: []store.Filter{{Field: "course", Operator: store.In,
		Value: store.Subquery{Collection: "enrollments", Select: "course"}}}}
	if _, err := f.gw.As(f.admin.p).List(ctx, sub); !errors.Is(err, store.ErrInvalidInput) {
		t.Errorf("subquery filter: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.gw.As(f.admin.p).List(ctx, store.Query{Collection: "_users"}); !errors.Is(err, gateway.ErrUnknownCollection) {
		t.Errorf("engine collection: want ErrUnknownCollection, got %v", err)
	}
}

// Writes through As(p) run the HTTP write pipeline: authorization with the same
// error kinds, actor stamping, field rules, validation, hooks, revisions.
func TestEmbed_ScopedWrites(t *testing.T) {
	hooks := gateway.NewHookRegistry()
	var hooked string
	hooks.On("notes", gateway.BeforeCreate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
		hooked = hc.Principal.ID
		return rec, nil
	})
	gw, base, db := embedServer(t, embedSchema, gateway.Options{Hooks: hooks})
	ctx := context.Background()
	student := newCaller(t, base, db, "student")
	outsider := newCaller(t, base, db, "outsider")
	anon := caller{name: "anonymous"}

	input := store.Record{"body": "hello"}
	rec, err := gw.As(student.p).Create(ctx, "notes", input)
	if err != nil {
		t.Fatalf("student create: %v", err)
	}
	if rec["created_by"] != student.p.ID || hooked != student.p.ID {
		t.Errorf("actor/hook principal: created_by=%v hook saw %q, want %s", rec["created_by"], hooked, student.p.ID)
	}
	if _, touched := input["id"]; touched || len(input) != 1 {
		t.Errorf("Create modified the caller's map: %v", input)
	}
	id := rec["id"].(string)

	// The owner can read and update it; the HTTP API agrees.
	if _, err := gw.As(student.p).Get(ctx, "notes", id); err != nil {
		t.Errorf("owner Get: %v", err)
	}
	if st, _ := doAs(t, http.MethodGet, base+"/api/v1/notes/"+id, student.token, ""); st != http.StatusOK {
		t.Errorf("owner HTTP GET: %d", st)
	}
	if _, err := gw.As(student.p).Update(ctx, "notes", id, store.Record{"body": "edited"}, nil); err != nil {
		t.Errorf("owner Update: %v", err)
	}

	// Others: same error kinds as the HTTP statuses.
	if _, err := gw.As(outsider.p).Update(ctx, "notes", id, store.Record{"body": "x"}, nil); !errors.Is(err, gateway.ErrForbidden) {
		t.Errorf("outsider Update: want ErrForbidden (HTTP 403), got %v", err)
	}
	if _, err := gw.As(anon.p).Create(ctx, "notes", store.Record{"body": "x"}); !errors.Is(err, gateway.ErrUnauthenticated) {
		t.Errorf("anonymous Create: want ErrUnauthenticated (HTTP 401), got %v", err)
	}
	if _, err := gw.As(outsider.p).Get(ctx, "notes", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("outsider Get: want ErrNotFound (HTTP 404), got %v", err)
	}
	if _, err := gw.As(student.p).Update(ctx, "notes", id, store.Record{"body": 5}, nil); err == nil {
		t.Error("invalid body type should fail validation")
	}
	// A version precondition on a collection without `concurrency` is refused.
	v := int64(1)
	if _, err := gw.As(student.p).Update(ctx, "notes", id, store.Record{"body": "y"}, &v); !errors.Is(err, store.ErrInvalidInput) {
		t.Errorf("IfVersion without concurrency: want ErrInvalidInput, got %v", err)
	}

	// Soft delete, then the record is gone for its owner too; purge removes the row.
	if err := gw.As(student.p).Delete(ctx, "notes", id, nil, false); err != nil {
		t.Fatalf("owner Delete: %v", err)
	}
	if _, err := gw.As(student.p).Get(ctx, "notes", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("trashed note: want ErrNotFound, got %v", err)
	}
	if _, err := db.FindOne(ctx, "notes", id); err != nil {
		t.Errorf("soft delete should keep the row: %v", err)
	}
}

// Media through the Go API applies the HTTP rules: update rights on the asset, the
// extension allowlist, and the read check before signing.
func TestEmbed_Media(t *testing.T) {
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	// Writes to an asset restricted to its owner (the engine default is any
	// authenticated user).
	src := strings.Replace(signMediaSchema, "read: inherit", "read: inherit\n      update: { any: [admin, owner] }", 1)
	gw, base, db := embedServer(t, src, gateway.Options{Blob: bs, URLSigner: testSigner()})
	ctx := context.Background()
	admin := newCaller(t, base, db, "admin", "admin")
	bob := newCaller(t, base, db, "bob")

	_, up := uploadFileAs(t, base+"/__media", admin.token, "v.mp4", "video/mp4", []byte("ORIGINAL"))
	asset := recordID(t, up)

	if n, err := gw.PutFile(ctx, admin.p, asset, "hls/index.m3u8", strings.NewReader("#EXTM3U")); err != nil || n != 7 {
		t.Fatalf("admin PutFile: n=%d err=%v", n, err)
	}
	if _, err := gw.PutFile(ctx, bob.p, asset, "hls/seg.ts", bytes.NewReader([]byte("x"))); !errors.Is(err, gateway.ErrForbidden) {
		t.Errorf("non-owner PutFile: want ErrForbidden, got %v", err)
	}
	if code := putRaw(t, base+"/__media/"+asset+"/files/hls/seg.ts", bob.token, "video/mp2t", []byte("x")); code != http.StatusForbidden {
		t.Errorf("non-owner HTTP PUT: %d, want 403 like the Go API", code)
	}
	if _, err := gw.PutFile(ctx, admin.p, asset, "x.html", strings.NewReader("<script>")); !errors.Is(err, gateway.ErrUnsupportedMediaType) {
		t.Errorf("html PutFile: want ErrUnsupportedMediaType, got %v", err)
	}

	u, err := gw.SignedURLAs(ctx, admin.p, asset, "hls/", "hls/index.m3u8", 0)
	if err != nil {
		t.Fatalf("admin SignedURLAs: %v", err)
	}
	if code, body, _ := getRaw(t, base+u, ""); code != http.StatusOK || string(body) != "#EXTM3U" {
		t.Errorf("signed URL fetch: %d %q", code, body)
	}
	// bob can't read the asset (inherit: nothing he can read references it).
	if _, err := gw.SignedURLAs(ctx, bob.p, asset, "hls/", "hls/index.m3u8", 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unreadable asset SignedURLAs: want ErrNotFound, got %v", err)
	}
	// The unchecked variant still validates its input.
	if _, err := gw.SignedURL(asset, "hls", "hls/index.m3u8", 0); !errors.Is(err, store.ErrInvalidInput) {
		t.Errorf("bad scope SignedURL: want ErrInvalidInput, got %v", err)
	}
	if _, err := gw.SignedURL(asset, "", "", 0); err != nil {
		t.Errorf("system SignedURL: %v", err)
	}
}
