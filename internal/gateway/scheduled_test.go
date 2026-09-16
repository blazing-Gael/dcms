package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

const scheduledHTTPSchema = `
version: "1"
collections:
  stories:
    publishing: true
    events: true
    fields:
      title: { type: string, required: true }
`

func markerRows(t *testing.T, db store.Adapter) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{Collection: schema.ScheduledPublishesCollection, SkipCount: true})
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return page.Data
}

// A scheduled publish through the API arms a go-live marker in the transition's
// own transaction (issue #28), and unpublishing before the time clears it.
func TestScheduled_PublishArmsAndUnpublishClearsMarker(t *testing.T) {
	def, db := newDB(t, scheduledHTTPSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/stories", `{"title":"Later"}`)
	id := dataObj(t, b)["id"].(string)

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if st, _ := do(t, http.MethodPost, base+"/stories/"+id+"/publish", `{"at":"`+future+`"}`); st != http.StatusOK {
		t.Fatalf("scheduled publish: %d", st)
	}
	markers := markerRows(t, db)
	if len(markers) != 1 || markers[0][schema.ScheduledRecordID] != id {
		t.Fatalf("scheduled publish should arm exactly one marker for the record, got %#v", markers)
	}

	// An immediate publish (no future time) is live now, so it leaves no marker —
	// its own `published` event is the go-live signal.
	if st, _ := do(t, http.MethodPost, base+"/stories/"+id+"/publish", `{}`); st != http.StatusOK {
		t.Fatalf("immediate publish: %d", st)
	}
	if n := len(markerRows(t, db)); n != 0 {
		t.Fatalf("immediate publish should leave no marker, got %d", n)
	}

	// Re-arm, then unpublish → marker cleared.
	do(t, http.MethodPost, base+"/stories/"+id+"/publish", `{"at":"`+future+`"}`)
	if n := len(markerRows(t, db)); n != 1 {
		t.Fatalf("re-arm should leave one marker, got %d", n)
	}
	if st, _ := do(t, http.MethodPost, base+"/stories/"+id+"/unpublish", `{}`); st != http.StatusOK {
		t.Fatalf("unpublish: %d", st)
	}
	if n := len(markerRows(t, db)); n != 0 {
		t.Fatalf("unpublish should clear the marker, got %d", n)
	}
}
