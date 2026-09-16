package gateway_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// Issue #29: a forward many-to-many can be expanded on a LIST, batched (two
// queries for the whole page), instead of the old per-record N+1.

func TestM2M_ExpandOnList(t *testing.T) {
	def, db := newDB(t, m2mSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/tags", `{"name":"go"}`)
	goID := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/tags", `{"name":"db"}`)
	dbID := dataObj(t, b)["id"].(string)

	// Two posts with different tag sets.
	do(t, http.MethodPost, base+"/posts", `{"title":"A","tags":["`+goID+`","`+dbID+`"]}`)
	do(t, http.MethodPost, base+"/posts", `{"title":"B","tags":["`+goID+`"]}`)

	st, body := do(t, http.MethodGet, base+"/posts?expand=tags", "")
	if st != http.StatusOK {
		t.Fatalf("list expand: %d %v", st, body)
	}
	rows, _ := body["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want 2 posts, got %d", len(rows))
	}
	// Each row carries its tags inlined as objects (order-independent by title).
	counts := map[string]int{}
	for _, row := range rows {
		r := row.(map[string]any)
		tags, ok := r["tags"].([]any)
		if !ok {
			t.Fatalf("post %v has no expanded tags: %#v", r["title"], r)
		}
		counts[r["title"].(string)] = len(tags)
		for _, tg := range tags {
			if _, ok := tg.(map[string]any)["name"]; !ok {
				t.Fatalf("expanded tag is not an object: %#v", tg)
			}
		}
	}
	if counts["A"] != 2 || counts["B"] != 1 {
		t.Fatalf("wrong per-record tag counts: %#v", counts)
	}
	// No truncation on a small page.
	if meta, _ := body["meta"].(map[string]any); meta["expand_truncated"] != nil {
		t.Errorf("did not expect expand_truncated: %#v", meta["expand_truncated"])
	}
}

// The store caps a single Find at 100 rows, so a page whose total links (and
// distinct targets) exceed 100 must be paginated — otherwise some records would
// silently lose their relation. Here two posts share a pool of 105 tags so both
// the join fetch (135 links) and the target fetch (105 distinct) cross the cap,
// yet each post gets its full, untruncated set.
func TestM2M_ExpandOnListPaginatesPastStoreCap(t *testing.T) {
	def, db := newDB(t, m2mSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	const pool = 105
	ids := make([]string, 0, pool)
	for i := 0; i < pool; i++ {
		_, b := do(t, http.MethodPost, base+"/tags", fmt.Sprintf(`{"name":"t%d"}`, i))
		ids = append(ids, dataObj(t, b)["id"].(string))
	}
	do(t, http.MethodPost, base+"/posts", `{"title":"A","tags":["`+strings.Join(ids[0:70], `","`)+`"]}`)
	do(t, http.MethodPost, base+"/posts", `{"title":"B","tags":["`+strings.Join(ids[40:105], `","`)+`"]}`)

	_, body := do(t, http.MethodGet, base+"/posts?expand=tags&limit=10", "")
	counts := map[string]int{}
	for _, row := range body["data"].([]any) {
		r := row.(map[string]any)
		counts[r["title"].(string)] = len(r["tags"].([]any))
	}
	if counts["A"] != 70 || counts["B"] != 65 {
		t.Fatalf("pagination lost links: A=%d (want 70), B=%d (want 65)", counts["A"], counts["B"])
	}
	if meta := body["meta"].(map[string]any); meta["expand_truncated"] != nil {
		t.Errorf("no record exceeded the per-record cap; unexpected expand_truncated: %#v", meta["expand_truncated"])
	}
}

func TestM2M_ExpandOnListTruncates(t *testing.T) {
	def, db := newDB(t, m2mSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// One post linked to more tags than the per-record list cap (maxM2MListExpand
	// = 100), so its relation is truncated and reported in meta.
	const n = 101
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		_, b := do(t, http.MethodPost, base+"/tags", fmt.Sprintf(`{"name":"t%d"}`, i))
		ids = append(ids, dataObj(t, b)["id"].(string))
	}
	st, b := do(t, http.MethodPost, base+"/posts", `{"title":"Big","tags":["`+strings.Join(ids, `","`)+`"]}`)
	if st != http.StatusCreated {
		t.Fatalf("create big post: %d %v", st, b)
	}

	_, body := do(t, http.MethodGet, base+"/posts?expand=tags", "")
	rows := body["data"].([]any)
	tags := rows[0].(map[string]any)["tags"].([]any)
	if len(tags) != 100 {
		t.Fatalf("expected the relation capped at 100, got %d", len(tags))
	}
	meta := body["meta"].(map[string]any)
	trunc, ok := meta["expand_truncated"].([]any)
	if !ok || len(trunc) != 1 || trunc[0] != "tags" {
		t.Fatalf("expected meta.expand_truncated == [tags], got %#v", meta["expand_truncated"])
	}
}
