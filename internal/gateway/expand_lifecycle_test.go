package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// A batched m2m expansion must apply the target collection's lifecycle view, so a
// draft (not-yet-published) related record is not leaked to an anonymous reader
// through ?expand — the same filter a direct read of the target would apply
// (issues #29 + #12).
const expandLifecycleSchema = `
version: "1"
collections:
  topics:
    publishing: true
    fields:
      name: { type: string, required: true }
    access:
      read: public
  stories:
    fields:
      title: { type: string, required: true }
      topics: { type: relation, target: topics, many: true }
    access:
      read: public
`

func TestExpandM2M_HidesUnpublishedTargetsOnList(t *testing.T) {
	def, db := newDB(t, expandLifecycleSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// One published topic, one left as a draft.
	_, b := do(t, http.MethodPost, base+"/topics", `{"name":"live"}`)
	liveID := dataObj(t, b)["id"].(string)
	if st, _ := do(t, http.MethodPost, base+"/topics/"+liveID+"/publish", `{}`); st != http.StatusOK {
		t.Fatalf("publish topic: %d", st)
	}
	_, b = do(t, http.MethodPost, base+"/topics", `{"name":"draft"}`)
	draftID := dataObj(t, b)["id"].(string)

	// A story links both topics.
	do(t, http.MethodPost, base+"/stories", `{"title":"S","topics":["`+liveID+`","`+draftID+`"]}`)

	// Anonymous list expand: only the published topic is inlined; the draft is
	// filtered out by the target's lifecycle view, exactly as a direct read would.
	_, body := do(t, http.MethodGet, base+"/stories?expand=topics", "")
	rows := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 story, got %d", len(rows))
	}
	topics := rows[0].(map[string]any)["topics"].([]any)
	if len(topics) != 1 {
		t.Fatalf("expanded topics = %d, want 1 (draft hidden from anon)", len(topics))
	}
	if topics[0].(map[string]any)["name"] != "live" {
		t.Fatalf("visible topic = %#v, want the published one", topics[0])
	}
}
