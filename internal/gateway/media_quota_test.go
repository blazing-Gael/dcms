package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/config"
	"github.com/blazing-Gael/dcms/internal/gateway"
)

const quotaMediaSchema = `
version: "1"
auth:
  roles:
    editor: { label: Editor }
  session:
    ttl: 1h
collections:
  _media:
    access:
      read: authenticated
`

func TestMediaQuota_PerPrincipalCapWithRoleOverride(t *testing.T) {
	// Default cap 100 bytes; editors get 1000. Uploads are ~60 bytes each.
	srv, db := mediaAuthServer(t, quotaMediaSchema, gateway.Options{
		MediaQuota: &gateway.MediaQuotaOptions{Default: 100, Roles: map[string]int64{"editor": 1000}},
	})
	seedUser(t, db, "u@x.com", "pw-user-1234")
	seedUser(t, db, "ed@x.com", "pw-editor-123", "editor")
	user := login(t, srv.URL, "u@x.com", "pw-user-1234")
	ed := login(t, srv.URL, "ed@x.com", "pw-editor-123")
	media := srv.URL + "/__media"
	blob := []byte(strings.Repeat("x", 60))

	// The ordinary user's first upload fits under 100; the second (120 total) is refused.
	if st, _ := uploadFileAs(t, media, user, "a.bin", "application/octet-stream", blob); st != http.StatusCreated {
		t.Fatalf("first upload: got %d, want 201", st)
	}
	st, body := uploadFileAs(t, media, user, "b.bin", "application/octet-stream", blob)
	if st != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota upload: got %d, want 413 (%v)", st, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "QUOTA_EXCEEDED" {
		t.Fatalf("error code = %q, want QUOTA_EXCEEDED", code)
	}

	// An editor has the larger cap, so the same second upload succeeds for them.
	if st, _ := uploadFileAs(t, media, ed, "a.bin", "application/octet-stream", blob); st != http.StatusCreated {
		t.Fatalf("editor first upload: got %d, want 201", st)
	}
	if st, _ := uploadFileAs(t, media, ed, "b.bin", "application/octet-stream", blob); st != http.StatusCreated {
		t.Fatalf("editor second upload (under 1000): got %d, want 201", st)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"1024": 1024, "1KiB": 1024, "1KB": 1000, "2MiB": 2 << 20,
		"5GiB": 5 << 30, "unlimited": -1, "0": 0,
	}
	for in, want := range cases {
		got, err := config.ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "10XB", "-5"} {
		if _, err := config.ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) should error", bad)
		}
	}
}
