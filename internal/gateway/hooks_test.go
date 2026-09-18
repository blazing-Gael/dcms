package gateway_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Two collections: notes carry the hooks under test; audit is what a hook writes
// into, so we can prove a hook's own writes are transactional with the trigger.
const hookSchema = `
version: "1"
collections:
  notes:
    fields:
      title:   { type: string, required: true }
      slug:    { type: string }
      revised: { type: boolean }
  audit:
    fields:
      note_id: { type: string }
      action:  { type: string }
`

func TestHooks_BeforeCreateMutates(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().On("notes", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			// Derive slug from title if the client didn't set one.
			if rec["slug"] == nil || rec["slug"] == "" {
				rec["slug"] = strings.ToLower(rec["title"].(string))
			}
			return rec, nil
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})
	base := srv.URL + "/api/v1/notes"

	_, body := do(t, http.MethodPost, base, `{"title":"Hello"}`)
	if got := dataObj(t, body)["slug"]; got != "hello" {
		t.Fatalf("BeforeCreate should derive slug, got %v", got)
	}
}

func TestHooks_BeforeCreateRejects(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().On("notes", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			if rec["title"] == "banned" {
				return nil, &gateway.HookError{Status: http.StatusUnprocessableEntity, Code: "BANNED", Message: "that title is not allowed"}
			}
			return rec, nil
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})
	base := srv.URL + "/api/v1/notes"

	st, body := do(t, http.MethodPost, base, `{"title":"banned"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("rejected create should be 422, got %d", st)
	}
	if body["error"] == nil || !strings.Contains(bodyErr(body), "not allowed") {
		t.Fatalf("rejection message not surfaced: %v", body)
	}
	// The reject rolled back — nothing was created.
	if n := countRows(t, db, "notes"); n != 0 {
		t.Fatalf("a rejected create must not persist, found %d rows", n)
	}
}

func TestHooks_BeforeCreateRejectDefaults422(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().On("notes", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			return nil, &gateway.HookError{Message: "nope"} // no Status → default 422
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks})
	if st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/notes", `{"title":"x"}`); st != http.StatusUnprocessableEntity {
		t.Fatalf("HookError without status should default to 422, got %d", st)
	}
}

func TestHooks_AfterCreateSideEffectIsTransactional(t *testing.T) {
	def, db := newDB(t, hookSchema)
	// AfterCreate writes an audit row through the hook's transactional store handle.
	hooks := gateway.NewHookRegistry().On("notes", gateway.AfterCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			_, err := hc.Store.Create(ctx, store.WriteInput{Collection: "audit", Data: store.Record{
				"note_id": rec["id"], "action": "created",
			}})
			return rec, err
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})

	_, body := do(t, http.MethodPost, srv.URL+"/api/v1/notes", `{"title":"Hi"}`)
	id := dataObj(t, body)["id"].(string)

	rows := findWhere(t, db, "audit", "note_id", id)
	if len(rows) != 1 || rows[0]["action"] != "created" {
		t.Fatalf("AfterCreate audit row missing/wrong: %#v", rows)
	}
}

func TestHooks_AfterCreateFailureRollsBackTheWrite(t *testing.T) {
	def, db := newDB(t, hookSchema)
	// An AfterCreate that fails must roll back the note itself — the whole write is
	// one transaction (ADR-0031 §2).
	hooks := gateway.NewHookRegistry().On("notes", gateway.AfterCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			return nil, &gateway.HookError{Status: http.StatusConflict, Message: "post-check failed"}
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks})

	if st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/notes", `{"title":"Hi"}`); st != http.StatusConflict {
		t.Fatalf("AfterCreate failure should surface as 409, got %d", st)
	}
	if n := countRows(t, db, "notes"); n != 0 {
		t.Fatalf("AfterCreate failure must roll back the note, found %d rows", n)
	}
}

func TestHooks_BeforeUpdateAndAfterUpdate(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().
		On("notes", gateway.BeforeUpdate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			rec["revised"] = true // stamp every update
			return rec, nil
		}).
		On("notes", gateway.AfterUpdate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			_, err := hc.Store.Create(ctx, store.WriteInput{Collection: "audit", Data: store.Record{"note_id": rec["id"], "action": "updated"}})
			return rec, err
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})
	base := srv.URL + "/api/v1/notes"

	_, body := do(t, http.MethodPost, base, `{"title":"A"}`)
	id := dataObj(t, body)["id"].(string)

	_, upd := do(t, http.MethodPatch, base+"/"+id, `{"title":"B"}`)
	if dataObj(t, upd)["revised"] != true {
		t.Fatalf("BeforeUpdate should have stamped revised=true: %#v", dataObj(t, upd))
	}
	if rows := findWhere(t, db, "audit", "note_id", id); len(rows) != 1 || rows[0]["action"] != "updated" {
		t.Fatalf("AfterUpdate audit row missing/wrong: %#v", rows)
	}
}

func TestHooks_BeforeDeleteRejectsAndAfterDeleteRuns(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().
		On("notes", gateway.BeforeDelete, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			if rec["title"] == "locked" {
				return nil, &gateway.HookError{Status: http.StatusForbidden, Message: "record is locked"}
			}
			return rec, nil
		}).
		On("notes", gateway.AfterDelete, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			_, err := hc.Store.Create(ctx, store.WriteInput{Collection: "audit", Data: store.Record{"note_id": rec["id"], "action": "deleted"}})
			return rec, err
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks})
	base := srv.URL + "/api/v1/notes"

	// A locked record can't be deleted; it survives.
	_, b1 := do(t, http.MethodPost, base, `{"title":"locked"}`)
	locked := dataObj(t, b1)["id"].(string)
	if st, _ := do(t, http.MethodDelete, base+"/"+locked, ""); st != http.StatusForbidden {
		t.Fatalf("BeforeDelete reject should be 403, got %d", st)
	}
	if countRows(t, db, "notes") != 1 {
		t.Fatal("rejected delete must not remove the row")
	}

	// A normal record deletes, and AfterDelete records it.
	_, b2 := do(t, http.MethodPost, base, `{"title":"free"}`)
	free := dataObj(t, b2)["id"].(string)
	if st, _ := do(t, http.MethodDelete, base+"/"+free, ""); st != http.StatusNoContent {
		t.Fatalf("normal delete should be 204, got %d", st)
	}
	if rows := findWhere(t, db, "audit", "note_id", free); len(rows) != 1 || rows[0]["action"] != "deleted" {
		t.Fatalf("AfterDelete audit row missing/wrong: %#v", rows)
	}
}

func TestHooks_SeeVerifiedPrincipal(t *testing.T) {
	var seen string
	hooks := gateway.NewHookRegistry().On("notes", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			seen = hc.Principal.ID // the verified caller
			if !hc.Principal.Authenticated {
				return nil, &gateway.HookError{Status: http.StatusUnauthorized, Message: "must be signed in"}
			}
			return rec, nil
		})
	// newAccountsServer uses authSchema (notes: create authenticated) and wires a
	// session Authenticator, so a real verified principal reaches the hook.
	base, db := newAccountsServer(t, gateway.Options{Hooks: hooks})
	userID := seedUserID(t, db, "u@x.com", "correcthorse", "author")
	token := login(t, base, "u@x.com", "correcthorse")

	if st, _ := doAs(t, http.MethodPost, base+"/api/v1/notes", token, `{"body":"Hi"}`); st != http.StatusCreated {
		t.Fatalf("authenticated create should be 201, got %d", st)
	}
	if seen != userID {
		t.Fatalf("hook saw principal %q, want the verified user %q", seen, userID)
	}
}

// ── small DB assertions ───────────────────────────────────────────────────────

func bodyErr(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

func countRows(t *testing.T, db store.Adapter, collection string) int {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{Collection: collection, SkipCount: true})
	if err != nil {
		t.Fatalf("count %s: %v", collection, err)
	}
	return len(page.Data)
}

func findWhere(t *testing.T, db store.Adapter, collection, field, value string) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{
		Collection: collection,
		Filters:    []store.Filter{{Field: field, Operator: store.Eq, Value: value}},
		SkipCount:  true,
	})
	if err != nil {
		t.Fatalf("find %s where %s=%s: %v", collection, field, value, err)
	}
	return page.Data
}

func TestHooks_OrderAndChaining(t *testing.T) {
	def, db := newDB(t, hookSchema)
	hooks := gateway.NewHookRegistry().
		On("notes", gateway.BeforeCreate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			rec["title"] = rec["title"].(string) + "-1"
			return rec, nil
		}).
		On("notes", gateway.BeforeCreate, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			rec["title"] = rec["title"].(string) + "-2"
			return rec, nil
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})

	_, body := do(t, http.MethodPost, srv.URL+"/api/v1/notes", `{"title":"x"}`)
	if got := dataObj(t, body)["title"]; got != "x-1-2" {
		t.Fatalf("hooks should chain in registration order, got %v", got)
	}
}

func TestHooks_UnhookedCollectionUnaffected(t *testing.T) {
	def, db := newDB(t, hookSchema)
	// A hook only on notes must not perturb writes to audit.
	hooks := gateway.NewHookRegistry().On("notes", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			return nil, &gateway.HookError{Message: "notes are closed"}
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})
	if st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/audit", `{"action":"ok"}`); st != http.StatusCreated {
		t.Fatalf("a collection with no hooks should be unaffected, got %d", st)
	}
}

// ── the two special write paths ───────────────────────────────────────────────

const softDeleteHookSchema = `
version: "1"
collections:
  notes:
    soft_delete: true
    fields:
      title: { type: string, required: true }
  audit:
    fields:
      note_id: { type: string }
      action:  { type: string }
`

// A soft delete (which is an update under the hood) must still fire the delete
// hooks, since it is semantically a delete (ADR-0031).
func TestHooks_FireOnSoftDelete(t *testing.T) {
	def, db := newDB(t, softDeleteHookSchema)
	var before, after bool
	hooks := gateway.NewHookRegistry().
		On("notes", gateway.BeforeDelete, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			before = true
			if rec["title"] == "locked" {
				return nil, &gateway.HookError{Status: http.StatusForbidden, Message: "locked"}
			}
			return rec, nil
		}).
		On("notes", gateway.AfterDelete, func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			after = true
			_, err := hc.Store.Create(ctx, store.WriteInput{Collection: "audit", Data: store.Record{"note_id": rec["id"], "action": "soft-deleted"}})
			return rec, err
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks})
	base := srv.URL + "/api/v1/notes"

	// Locked → BeforeDelete rejects, nothing trashed.
	_, bl := do(t, http.MethodPost, base, `{"title":"locked"}`)
	if st, _ := do(t, http.MethodDelete, base+"/"+dataObj(t, bl)["id"].(string), ""); st != http.StatusForbidden {
		t.Fatalf("soft-delete reject should be 403, got %d", st)
	}

	// Free → soft-deleted; both hooks ran and the audit row committed with it.
	_, bf := do(t, http.MethodPost, base, `{"title":"free"}`)
	id := dataObj(t, bf)["id"].(string)
	if st, _ := do(t, http.MethodDelete, base+"/"+id, ""); st != http.StatusNoContent {
		t.Fatalf("soft-delete should be 204, got %d", st)
	}
	if !before || !after {
		t.Fatalf("both delete hooks should fire on soft delete (before=%v after=%v)", before, after)
	}
	if rows := findWhere(t, db, "audit", "note_id", id); len(rows) != 1 {
		t.Fatalf("AfterDelete audit row should commit with the soft delete: %#v", rows)
	}
}

const inlineHookSchema = `
version: "1"
collections:
  authors:
    fields:
      name: { type: string, required: true }
  books:
    fields:
      title:  { type: string, required: true }
      tag:    { type: string }
      author: { type: relation, target: authors }
`

// The inline-related-create path opens its own transaction; hooks must fire there
// too (ADR-0031 — no silent gaps across write paths).
func TestHooks_FireOnInlineRelationCreate(t *testing.T) {
	def, db := newDB(t, inlineHookSchema)
	hooks := gateway.NewHookRegistry().On("books", gateway.BeforeCreate,
		func(ctx context.Context, hc gateway.HookContext, rec store.Record) (store.Record, error) {
			rec["tag"] = "hooked"
			return rec, nil
		})
	srv := mount(t, def, db, gateway.Options{Hooks: hooks, ValidateResponses: true})

	// author supplied inline → the inline-create Tx path.
	_, body := do(t, http.MethodPost, srv.URL+"/api/v1/books", `{"title":"T","author":{"name":"Ada"}}`)
	if got := dataObj(t, body)["tag"]; got != "hooked" {
		t.Fatalf("BeforeCreate should run on the inline-relation path, tag=%v", got)
	}
}
