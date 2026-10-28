package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Issue #70: with no `_media` update/delete rule declared, only the uploader or an
// admin may replace, edit, delete or add derived files to an upload. Upload and
// read are unchanged.

const mediaWritesSchema = `
version: "1"
auth:
  roles:
    admin:     { label: Administrator }
    superuser: { label: Superuser }
  session: { ttl: 1h }
collections:
  notes:
    fields: { body: { type: string } }
`

// mediaWriteCase is one write path against an asset, returning the HTTP status.
type mediaWriteCase struct {
	name string
	do   func(t *testing.T, base, id, token string) int
}

var mediaWriteCases = []mediaWriteCase{
	{"replace", func(t *testing.T, base, id, token string) int {
		st, _ := uploadFileAs(t, base+"/__media/"+id, token, "new.png", "image/png", []byte("NEW"))
		return st
	}},
	{"patch", func(t *testing.T, base, id, token string) int {
		st, _ := doAs(t, http.MethodPatch, base+"/__media/"+id, token, `{"alt":"x"}`)
		return st
	}},
	{"derived put", func(t *testing.T, base, id, token string) int {
		return putRaw(t, base+"/__media/"+id+"/files/thumb.png", token, "image/png", []byte("T"))
	}},
	// delete last: it removes the asset.
	{"delete", func(t *testing.T, base, id, token string) int {
		st, _ := doAs(t, http.MethodDelete, base+"/__media/"+id, token, "")
		return st
	}},
}

func mediaWritesServer(t *testing.T, src string, opts gateway.Options) (*gateway.Server, string, store.Adapter) {
	t.Helper()
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	opts.Blob = bs
	return embedServer(t, src, opts)
}

// upload stores a fresh asset as token and returns its id.
func uploadAsset(t *testing.T, base, token string) string {
	t.Helper()
	st, body := uploadFileAs(t, base+"/__media", token, "a.png", "image/png", []byte("ORIGINAL"))
	if st != http.StatusCreated {
		t.Fatalf("upload: %d %v", st, body)
	}
	return recordID(t, body)
}

func TestMediaWrites_OwnerOrAdminByDefault(t *testing.T) {
	_, base, db := mediaWritesServer(t, mediaWritesSchema, gateway.Options{})
	admin := newCaller(t, base, db, "admin", "admin")
	alice := newCaller(t, base, db, "alice")
	bob := newCaller(t, base, db, "bob")

	for _, c := range mediaWriteCases {
		t.Run(c.name, func(t *testing.T) {
			// Upload is still open to any signed-in user.
			id := uploadAsset(t, base, alice.token)

			if st := c.do(t, base, id, ""); st != http.StatusUnauthorized {
				t.Errorf("anonymous: %d, want 401", st)
			}
			if st := c.do(t, base, id, bob.token); st != http.StatusForbidden {
				t.Errorf("non-owner: %d, want 403", st)
			}
			if st := c.do(t, base, id, alice.token); st >= 300 {
				t.Errorf("owner: %d, want 2xx", st)
			}
			// A second asset, so delete has something left for the admin.
			id2 := uploadAsset(t, base, alice.token)
			if st := c.do(t, base, id2, admin.token); st >= 300 {
				t.Errorf("admin: %d, want 2xx", st)
			}
			// Reads stay public.
			if st, _ := do(t, http.MethodGet, base+"/__media/"+id2, ""); c.name != "delete" && st != http.StatusOK {
				t.Errorf("anonymous read: %d, want 200", st)
			}
		})
	}
}

// The default follows the configured admin roles, not a role literally named admin.
func TestMediaWrites_ConfiguredAdminRoles(t *testing.T) {
	_, base, db := mediaWritesServer(t, mediaWritesSchema, gateway.Options{AdminRoles: []string{"superuser"}})
	su := newCaller(t, base, db, "su", "superuser")
	plainAdmin := newCaller(t, base, db, "root", "admin")
	alice := newCaller(t, base, db, "alice")

	for _, c := range mediaWriteCases {
		t.Run(c.name, func(t *testing.T) {
			id := uploadAsset(t, base, alice.token)
			if st := c.do(t, base, id, plainAdmin.token); st != http.StatusForbidden {
				t.Errorf("role 'admin' not in admin_roles: %d, want 403", st)
			}
			if st := c.do(t, base, id, su.token); st >= 300 {
				t.Errorf("configured admin role: %d, want 2xx", st)
			}
		})
	}
}

// A declared rule replaces the default: `authenticated` restores the old any-user
// behaviour (a shared editorial library).
func TestMediaWrites_DeclaredRuleOverrides(t *testing.T) {
	src := mediaWritesSchema + `  _media:
    access:
      update: authenticated
      delete: authenticated
`
	_, base, db := mediaWritesServer(t, src, gateway.Options{})
	alice := newCaller(t, base, db, "alice")
	bob := newCaller(t, base, db, "bob")

	for _, c := range mediaWriteCases {
		t.Run(c.name, func(t *testing.T) {
			id := uploadAsset(t, base, alice.token)
			if st := c.do(t, base, id, bob.token); st >= 300 {
				t.Errorf("non-owner under `authenticated`: %d, want 2xx", st)
			}
		})
	}
}

// Declaring only update keeps the owner-or-admin default for delete.
func TestMediaWrites_PartialOverrideKeepsOtherDefault(t *testing.T) {
	src := mediaWritesSchema + `  _media:
    access:
      update: authenticated
`
	_, base, db := mediaWritesServer(t, src, gateway.Options{})
	alice := newCaller(t, base, db, "alice")
	bob := newCaller(t, base, db, "bob")

	id := uploadAsset(t, base, alice.token)
	if st, _ := doAs(t, http.MethodPatch, base+"/__media/"+id, bob.token, `{"alt":"x"}`); st != http.StatusOK {
		t.Errorf("declared update: non-owner PATCH %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodDelete, base+"/__media/"+id, bob.token, ""); st != http.StatusForbidden {
		t.Errorf("default delete: non-owner DELETE %d, want 403", st)
	}
}

// An asset with no recorded uploader (written through the privileged store, e.g.
// by a hook or an import) is admin-only.
func TestMediaWrites_OwnerlessAssetIsAdminOnly(t *testing.T) {
	_, base, db := mediaWritesServer(t, mediaWritesSchema, gateway.Options{})
	admin := newCaller(t, base, db, "admin", "admin")
	alice := newCaller(t, base, db, "alice")

	rec, err := db.Create(context.Background(), store.WriteInput{Collection: "_media", Data: store.Record{
		"filename": "sys.png", "content_type": "image/png", "size": 3, "storage_key": "sys-key",
	}})
	if err != nil {
		t.Fatalf("seed ownerless media: %v", err)
	}
	id := rec["id"].(string)

	if st, _ := doAs(t, http.MethodPatch, base+"/__media/"+id, alice.token, `{"alt":"x"}`); st != http.StatusForbidden {
		t.Errorf("user PATCH of ownerless asset: %d, want 403", st)
	}
	if st, _ := doAs(t, http.MethodPatch, base+"/__media/"+id, admin.token, `{"alt":"x"}`); st != http.StatusOK {
		t.Errorf("admin PATCH of ownerless asset: %d, want 200", st)
	}
}

// The Go API gets the same default as HTTP.
func TestMediaWrites_PutFileParity(t *testing.T) {
	gw, base, db := mediaWritesServer(t, mediaWritesSchema, gateway.Options{})
	ctx := context.Background()
	admin := newCaller(t, base, db, "admin", "admin")
	alice := newCaller(t, base, db, "alice")
	bob := newCaller(t, base, db, "bob")
	id := uploadAsset(t, base, alice.token)

	if _, err := gw.PutFile(ctx, bob.p, id, "seg.ts", strings.NewReader("x")); !errors.Is(err, gateway.ErrForbidden) {
		t.Errorf("non-owner PutFile: want ErrForbidden, got %v", err)
	}
	if _, err := gw.PutFile(ctx, alice.p, id, "seg.ts", strings.NewReader("x")); err != nil {
		t.Errorf("owner PutFile: %v", err)
	}
	if _, err := gw.PutFile(ctx, admin.p, id, "seg2.ts", strings.NewReader("x")); err != nil {
		t.Errorf("admin PutFile: %v", err)
	}
}
