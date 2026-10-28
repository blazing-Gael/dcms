package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Edge cases of the embedding API (ADR-0038) beyond the parity tables in
// embed_test.go: preview isolation, concurrency, field rules, transitions, query
// guards, deletes, hooks, and media failure modes.

const embedEdgeSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Administrator }
    editor: { label: Editor }
  session: { ttl: 1h }
collections:
  stories:
    publishing: true
    concurrency: true
    fields:
      title: { type: string, required: true }
      secret:
        type: string
        access: { read: [admin], write: [admin] }
      review:
        type: enum
        values: [writing, submitted, approved]
        default: writing
        access:
          write:
            rules:
              - who: owner
                from: [writing]
                to:   [writing, submitted]
              - who: [admin, editor]
    access:
      read:   public
      create: authenticated
      update: { any: [admin, editor, owner] }
      delete: [admin]
  contacts:
    soft_delete: true
    fields:
      name:  { type: string }
      phone:
        type: string
        access: { read: owner }
    access:
      read:   public
      create: authenticated
      delete: owner
  tags:
    fields: { name: { type: string } }
    access: { read: public, create: [admin], delete: [admin] }
  tagged:
    fields: { tag: { type: relation, target: tags, required: true } }
    access: { read: public, create: [admin] }
  vault:
    fields: { name: { type: string } }
    access: { read: [admin], create: [admin] }
`

func edgeServer(t *testing.T, opts gateway.Options) (*gateway.Server, string, caller, caller, caller) {
	t.Helper()
	gw, base, db := embedServer(t, embedEdgeSchema, opts)
	gwDBs[gw] = db
	return gw, base, newCaller(t, base, db, "admin", "admin"), newCaller(t, base, db, "writer"), newCaller(t, base, db, "editor", "editor")
}

// A preview token or ?status on an enclosing HTTP request must not widen what the
// Go API sees: As(p) and Can always use the default, published-only view.
func TestEmbedEdge_PreviewTokenDoesNotWidenGoAPI(t *testing.T) {
	var gw *gateway.Server
	var goErr error
	var goCan bool
	route := gateway.CustomRoute{Method: http.MethodGet, Path: "/peek/{id}", Handler: func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/peek/"):]
		_, goErr = gw.As(auth.Principal{}).Get(r.Context(), "stories", id)
		goCan, _ = gw.Can(r.Context(), auth.Principal{}, schema.ActionRead, "stories", id)
		w.WriteHeader(http.StatusNoContent)
	}}
	g, base, admin, _, _ := edgeServer(t, gateway.Options{PreviewToken: "tok", Routes: []gateway.CustomRoute{route}})
	gw = g

	_, b := doAs(t, http.MethodPost, base+"/api/v1/stories", admin.token, `{"title":"draft"}`)
	draft := recordID(t, b)

	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/stories/"+draft+"?status=any", nil)
	req.Header.Set("X-DCMS-Preview", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP preview of the draft: %d, want 200 (token works over HTTP)", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, base+"/peek/"+draft+"?status=any", nil)
	req.Header.Set("X-DCMS-Preview", "tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !errors.Is(goErr, store.ErrNotFound) || goCan {
		t.Errorf("Go API inside a token-carrying request: Get err %v, Can %v; want ErrNotFound, false", goErr, goCan)
	}
}

// IfVersion: a stale version is ErrVersionConflict, the current one succeeds and
// bumps the version.
func TestEmbedEdge_OptimisticConcurrency(t *testing.T) {
	gw, _, admin, _, _ := edgeServer(t, gateway.Options{})
	ctx := context.Background()
	s := gw.As(admin.p)
	rec, err := s.Create(ctx, "stories", store.Record{"title": "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := rec["id"].(string)
	v := toInt64(rec[schema.ConcurrencyVersion])

	stale := v - 1
	if _, err := s.Update(ctx, "stories", id, store.Record{"title": "b"}, &stale); !errors.Is(err, gateway.ErrVersionConflict) {
		t.Fatalf("stale version: want ErrVersionConflict, got %v", err)
	}
	updated, err := s.Update(ctx, "stories", id, store.Record{"title": "b"}, &v)
	if err != nil {
		t.Fatalf("current version: %v", err)
	}
	if got := toInt64(updated[schema.ConcurrencyVersion]); got != v+1 {
		t.Errorf("version after update = %d, want %d", got, v+1)
	}
	if err := s.Delete(ctx, "stories", id, &v, false); !errors.Is(err, gateway.ErrVersionConflict) {
		t.Errorf("stale delete: want ErrVersionConflict, got %v", err)
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return -1
}

// Write-field rules and value-scoped transitions apply to As(p) writes exactly as
// over HTTP: an unwritable field is dropped, a forbidden transition is ErrForbidden,
// and a returned record is masked for its caller.
func TestEmbedEdge_FieldRulesAndTransitions(t *testing.T) {
	gw, _, _, writer, editor := edgeServer(t, gateway.Options{})
	ctx := context.Background()

	rec, err := gw.As(writer.p).Create(ctx, "stories", store.Record{"title": "mine", "secret": "planted"})
	if err != nil {
		t.Fatalf("writer create: %v", err)
	}
	id := rec["id"].(string)
	if _, leaked := rec["secret"]; leaked {
		t.Errorf("writer's own create result shows the admin-only field: %v", rec)
	}
	// The story is an unpublished draft (hidden from Get), so check the stored row.
	full, err := gwDB(t, gw).FindOne(ctx, "stories", id)
	if err != nil {
		t.Fatalf("stored row: %v", err)
	}
	if full["secret"] != nil && full["secret"] != "" {
		t.Errorf("writer set an admin-only field: secret=%v", full["secret"])
	}

	// Unwritable field on update: dropped, no error.
	if _, err := gw.As(writer.p).Update(ctx, "stories", id, store.Record{"secret": "x"}, nil); err != nil {
		t.Errorf("unwritable field should be dropped silently, got %v", err)
	}

	// Transitions: owner may submit, not approve; an editor may approve.
	if _, err := gw.As(writer.p).Update(ctx, "stories", id, store.Record{"review": "approved"}, nil); !errors.Is(err, gateway.ErrForbidden) {
		t.Errorf("owner approving: want ErrForbidden, got %v", err)
	}
	if _, err := gw.As(writer.p).Update(ctx, "stories", id, store.Record{"review": "submitted"}, nil); err != nil {
		t.Errorf("owner submitting: %v", err)
	}
	if _, err := gw.As(editor.p).Update(ctx, "stories", id, store.Record{"review": "approved"}, nil); err != nil {
		t.Errorf("editor approving: %v", err)
	}
}

// Query guards: a masked field nested in And/Or is still caught; an owner-scoped
// field filter can't match other people's rows even inside an Or; a projection of
// a masked field stays masked; ReadFilters reproduces As(p).List on the privileged
// store; a role-gated collection gives ok=false.
func TestEmbedEdge_QueryGuards(t *testing.T) {
	gw, base, admin, writer, editor := edgeServer(t, gateway.Options{})
	ctx := context.Background()
	_ = base

	if _, err := gw.As(writer.p).Create(ctx, "contacts", store.Record{"name": "w", "phone": "111"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.As(editor.p).Create(ctx, "contacts", store.Record{"name": "e", "phone": "222"}); err != nil {
		t.Fatal(err)
	}

	// Writer probes the editor's phone through an Or: the owner scope confines it.
	probe := store.Query{Collection: "contacts", Filters: []store.Filter{{Operator: store.Or, Value: []store.Filter{
		{Field: "phone", Operator: store.Eq, Value: "222"},
		{Field: "name", Operator: store.Eq, Value: "nobody"},
	}}}}
	page, err := gw.As(writer.p).List(ctx, probe)
	if err != nil || len(page.Data) != 0 {
		t.Errorf("Or-nested probe of another's phone: %d rows, err %v; want 0", len(page.Data), err)
	}
	// Anonymous can't reference the owner-scoped field at all, nested or not.
	var ve *store.ValidationError
	if _, err := gw.As(auth.Principal{}).List(ctx, probe); !errors.As(err, &ve) {
		t.Errorf("anonymous nested masked filter: want ValidationError, got %v", err)
	}
	nestedAnd := store.Query{Collection: "stories", Filters: []store.Filter{{Operator: store.And, Value: []store.Filter{
		{Field: "secret", Operator: store.Eq, Value: "x"},
	}}}}
	if _, err := gw.As(writer.p).List(ctx, nestedAnd); !errors.As(err, &ve) {
		t.Errorf("And-nested admin-only field: want ValidationError, got %v", err)
	}
	if _, err := gw.As(writer.p).List(ctx, store.Query{Collection: "stories", Filters: []store.Filter{{Field: "nope", Operator: store.Eq, Value: 1}}}); !errors.As(err, &ve) {
		t.Errorf("unknown field: want ValidationError, got %v", err)
	}

	// Projection of a masked field: every other owner's phone is removed.
	page, err = gw.As(writer.p).List(ctx, store.Query{Collection: "contacts", Fields: []string{"id", "name", "phone", "created_by"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Data {
		if r["name"] == "e" {
			if _, leaked := r["phone"]; leaked {
				t.Errorf("projection leaked another owner's phone: %v", r)
			}
		}
	}

	// ReadFilters on the privileged store reproduces the scoped list.
	if _, err := gw.As(admin.p).Create(ctx, "stories", store.Record{"title": "draft only"}); err != nil {
		t.Fatal(err)
	}
	filters, ok, err := gw.ReadFilters(ctx, writer.p, "stories")
	if !ok || err != nil {
		t.Fatalf("ReadFilters: ok=%v err=%v", ok, err)
	}
	priv, err := gwDB(t, gw).Find(ctx, store.Query{Collection: "stories", Filters: filters})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := gw.As(writer.p).List(ctx, store.Query{Collection: "stories"})
	if err != nil {
		t.Fatal(err)
	}
	if len(priv.Data) != len(scoped.Data) {
		t.Errorf("ReadFilters on the privileged store: %d rows, As(p).List: %d", len(priv.Data), len(scoped.Data))
	}
	if _, ok, err := gw.ReadFilters(ctx, writer.p, "vault"); ok || err != nil {
		t.Errorf("role-gated ReadFilters: ok=%v err=%v; want false, nil", ok, err)
	}
	if _, err := gw.As(writer.p).List(ctx, store.Query{Collection: "vault"}); !errors.Is(err, gateway.ErrForbidden) {
		t.Errorf("role-gated List: want ErrForbidden, got %v", err)
	}
}

// gwDBs maps a test gateway to the store behind it (recorded by edgeServer), so a
// test can compare the Go API with a privileged query.
var gwDBs = map[*gateway.Server]store.Adapter{}

func gwDB(t *testing.T, gw *gateway.Server) store.Adapter {
	t.Helper()
	db, ok := gwDBs[gw]
	if !ok {
		t.Fatal("gateway not built by edgeServer")
	}
	return db
}

// Deletes: hard delete of a referenced record is ErrReferenced (HTTP 409); a
// soft-delete collection trashes unless Purge, which removes the row.
func TestEmbedEdge_Deletes(t *testing.T) {
	gw, base, db := embedServer(t, embedEdgeSchema, gateway.Options{})
	gwDBs[gw] = db
	ctx := context.Background()
	admin := newCaller(t, base, db, "admin", "admin")
	writer := newCaller(t, base, db, "writer")

	tag, _ := gw.As(admin.p).Create(ctx, "tags", store.Record{"name": "go"})
	tagID := tag["id"].(string)
	if _, err := gw.As(admin.p).Create(ctx, "tagged", store.Record{"tag": tagID}); err != nil {
		t.Fatal(err)
	}
	if err := gw.As(admin.p).Delete(ctx, "tags", tagID, nil, false); !errors.Is(err, gateway.ErrReferenced) {
		t.Errorf("referenced delete: want ErrReferenced, got %v", err)
	}
	if st, _ := doAs(t, http.MethodDelete, base+"/api/v1/tags/"+tagID, admin.token, ""); st != http.StatusConflict {
		t.Errorf("referenced HTTP delete: %d, want 409", st)
	}

	c, _ := gw.As(writer.p).Create(ctx, "contacts", store.Record{"name": "x"})
	id := c["id"].(string)
	if err := gw.As(writer.p).Delete(ctx, "contacts", id, nil, false); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := db.FindOne(ctx, "contacts", id); err != nil {
		t.Fatalf("trashed row should remain: %v", err)
	}
	c2, _ := gw.As(writer.p).Create(ctx, "contacts", store.Record{"name": "y"})
	id2 := c2["id"].(string)
	if err := gw.As(writer.p).Delete(ctx, "contacts", id2, nil, true); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := db.FindOne(ctx, "contacts", id2); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("purged row should be gone, got %v", err)
	}
}

// Can's argument handling: unknown and engine collections are errors; actions a
// collection doesn't support are false; an unknown action is an error.
func TestEmbedEdge_CanArguments(t *testing.T) {
	gw, _, admin, _, _ := edgeServer(t, gateway.Options{})
	ctx := context.Background()
	for _, coll := range []string{"nope", "_users", "_media", "_revisions"} {
		if _, err := gw.Can(ctx, admin.p, schema.ActionRead, coll, "x"); !errors.Is(err, gateway.ErrUnknownCollection) {
			t.Errorf("Can on %s: want ErrUnknownCollection, got %v", coll, err)
		}
		if _, err := gw.As(admin.p).Get(ctx, coll, "x"); !errors.Is(err, gateway.ErrUnknownCollection) {
			t.Errorf("Get on %s: want ErrUnknownCollection, got %v", coll, err)
		}
	}
	rec, _ := gw.As(admin.p).Create(ctx, "tags", store.Record{"name": "a"})
	id := rec["id"].(string)
	if ok, err := gw.Can(ctx, admin.p, schema.ActionHistory, "tags", id); ok || err != nil {
		t.Errorf("history on a collection without revisions: %v, %v; want false, nil", ok, err)
	}
	if ok, err := gw.Can(ctx, admin.p, schema.ActionPublish, "tags", id); ok || err != nil {
		t.Errorf("publish on a collection without publishing: %v, %v; want false, nil", ok, err)
	}
	if _, err := gw.Can(ctx, admin.p, schema.AccessAction("frobnicate"), "tags", id); err == nil {
		t.Error("unknown action should be an error")
	}
	// Allowed outright but absent: false, not true.
	if ok, err := gw.Can(ctx, admin.p, schema.ActionDelete, "tags", "missing"); ok || err != nil {
		t.Errorf("delete on a missing record: %v, %v; want false, nil", ok, err)
	}
}

// Hooks: a rejecting hook surfaces as *HookError through As(p); a write through
// As(p) from inside a hook fails fast with ErrInWriteHook instead of waiting out the
// database's busy timeout; reads from a hook still work.
func TestEmbedEdge_Hooks(t *testing.T) {
	hooks := gateway.NewHookRegistry()
	var gw *gateway.Server
	var nestedErr, readErr error
	hooks.On("tags", gateway.BeforeCreate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
		if rec["name"] == "reject" {
			return nil, &gateway.HookError{Status: http.StatusConflict, Code: "NOPE", Message: "rejected"}
		}
		if rec["name"] == "nested" {
			_, nestedErr = gw.As(hc.Principal).Create(ctx, "tags", store.Record{"name": "inner"})
			_, readErr = gw.As(hc.Principal).List(ctx, store.Query{Collection: "tags"})
		}
		return rec, nil
	})
	g, base, db := embedServer(t, embedEdgeSchema, gateway.Options{Hooks: hooks})
	gw = g
	ctx := context.Background()
	admin := newCaller(t, base, db, "admin", "admin")

	_, err := gw.As(admin.p).Create(ctx, "tags", store.Record{"name": "reject"})
	var he *gateway.HookError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Errorf("rejecting hook: want *HookError(409), got %v", err)
	}

	start := time.Now()
	if _, err := gw.As(admin.p).Create(ctx, "tags", store.Record{"name": "nested"}); err != nil {
		t.Fatalf("outer create: %v", err)
	}
	if !errors.Is(nestedErr, gateway.ErrInWriteHook) {
		t.Errorf("As(p) write inside a hook: want ErrInWriteHook, got %v", nestedErr)
	}
	if readErr != nil {
		t.Errorf("As(p) read inside a hook: %v", readErr)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("nested write took %v — it should fail fast, not wait on the lock", d)
	}
}

// Media failure modes through the Go API: no blob store, no signer, oversize, over
// quota, path traversal.
func TestEmbedEdge_MediaFailures(t *testing.T) {
	ctx := context.Background()

	// No blob store configured.
	gw, base, db := embedServer(t, signMediaSchema, gateway.Options{})
	admin := newCaller(t, base, db, "admin", "admin")
	if _, err := gw.PutFile(ctx, admin.p, "x", "a.ts", strings.NewReader("x")); !errors.Is(err, gateway.ErrMediaUnavailable) {
		t.Errorf("no blob: want ErrMediaUnavailable, got %v", err)
	}
	if _, err := gw.SignedURL("x", "", "", 0); !errors.Is(err, gateway.ErrMediaUnavailable) {
		t.Errorf("no blob SignedURL: want ErrMediaUnavailable, got %v", err)
	}

	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	gw, base, db = embedServer(t, signMediaSchema, gateway.Options{
		Blob:           bs,
		MaxUploadBytes: 1024, // also caps the primary multipart upload below
		MediaQuota:     &gateway.MediaQuotaOptions{Default: 1000},
	})
	admin = newCaller(t, base, db, "admin", "admin")
	_, up := uploadFileAs(t, base+"/__media", admin.token, "v.mp4", "video/mp4", []byte("ORIGINAL")) // 8 bytes
	asset := recordID(t, up)

	if _, err := gw.SignedURLAs(ctx, admin.p, asset, "", "", 0); !errors.Is(err, gateway.ErrSigningDisabled) {
		t.Errorf("no signer: want ErrSigningDisabled, got %v", err)
	}
	if _, err := gw.PutFile(ctx, admin.p, asset, "big.ts", strings.NewReader(strings.Repeat("x", 1025))); !errors.Is(err, gateway.ErrTooLarge) {
		t.Errorf("oversize: want ErrTooLarge, got %v", err)
	}
	for _, p := range []string{"../escape.ts", "/abs.ts", "a//b.ts", "a\\b.ts", "a/\n/b.ts", ""} {
		if _, err := gw.PutFile(ctx, admin.p, asset, p, strings.NewReader("x")); !errors.Is(err, store.ErrInvalidInput) {
			t.Errorf("path %q: want ErrInvalidInput, got %v", p, err)
		}
	}
	// 8 (primary) + 600 + 600 > 1000: the second file exceeds the owner's quota.
	if _, err := gw.PutFile(ctx, admin.p, asset, "a.ts", strings.NewReader(strings.Repeat("x", 600))); err != nil {
		t.Fatalf("within quota: %v", err)
	}
	if _, err := gw.PutFile(ctx, admin.p, asset, "b.ts", strings.NewReader(strings.Repeat("x", 600))); !errors.Is(err, gateway.ErrQuotaExceeded) {
		t.Errorf("over quota: want ErrQuotaExceeded, got %v", err)
	}
	if _, err := gw.PutFile(ctx, admin.p, "missing-asset", "a.ts", strings.NewReader("x")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing asset: want ErrNotFound, got %v", err)
	}
}

// Update and Create never modify the caller's map (the core edits its own copy).
func TestEmbedEdge_InputNotMutated(t *testing.T) {
	gw, _, admin, _, _ := edgeServer(t, gateway.Options{})
	ctx := context.Background()
	in := store.Record{"title": "t", "_status": "published", "id": "forced"}
	rec, err := gw.As(admin.p).Create(ctx, "stories", in)
	if err != nil {
		t.Fatal(err)
	}
	// Lifecycle fields change only via transitions; a client-supplied id is honoured
	// (ADR-0004), as over HTTP.
	if rec["_status"] == "published" || rec["id"] != "forced" {
		t.Errorf("want _status stripped and the supplied id kept: %v", rec)
	}
	if in["id"] != "forced" || in["_status"] != "published" || len(in) != 3 {
		t.Errorf("Create modified the caller's map: %v", in)
	}
	up := store.Record{"title": "u"}
	if _, err := gw.As(admin.p).Update(ctx, "stories", rec["id"].(string), up, nil); err != nil {
		t.Fatal(err)
	}
	if len(up) != 1 {
		t.Errorf("Update modified the caller's map: %v", up)
	}
}
