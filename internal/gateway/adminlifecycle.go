package gateway

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — 2B lifecycle (ADR-0035). The single biggest source of confusion for
// a non-technical client handed a DCMS site is "why isn't my new item showing on the
// website?" — the draft/published split is invisible in a raw CRUD panel. Here every
// record wears a plain-language status badge, and going live is one obvious button
// with a one-line explanation. Each action reuses the same authorized transition
// pipeline as the API (updateAndRevise → revisions + events + scheduling), so the
// panel stays a privileged client, never a backdoor.

// adminBadge is a status pill: a human label plus a CSS modifier class.
type adminBadge struct{ Label, CSS string }

// adminRowStatus derives a record's status pill for the list. Trashed wins over
// publishing state (a trashed record is off the site regardless of _status).
func adminRowStatus(cd schema.CollectionDef, rec store.Record) adminBadge {
	if cd.SoftDelete && isSet(rec[schema.LifecycleDeletedAt]) {
		return adminBadge{Label: "Trashed", CSS: "trashed"}
	}
	if !cd.Publishing {
		return adminBadge{}
	}
	switch status, _ := rec[schema.LifecycleStatus].(string); status {
	case schema.StatusPublished:
		if publishedAtReached(rec) {
			return adminBadge{Label: "Published", CSS: "published"}
		}
		return adminBadge{Label: "Scheduled", CSS: "scheduled"}
	case schema.StatusArchived:
		return adminBadge{Label: "Archived", CSS: "archived"}
	default:
		return adminBadge{Label: "Draft", CSS: "draft"}
	}
}

// adminAction is one lifecycle button on the edit page: a labeled POST to a
// transition route, with an optional confirm prompt for a destructive one.
type adminAction struct {
	Label, Verb, Style, Confirm string // Verb is the route suffix: publish/unpublish/…
}

// adminLifecycle is the edit page's status panel: the current badge, a one-line
// explanation aimed at a non-technical user, the available transition buttons, and
// whether a permanent delete is offered.
type adminLifecycle struct {
	Status     adminBadge
	Help       string
	Actions    []adminAction
	HardDelete bool
}

// adminLifecycleFor assembles the status panel for a record, offering only the
// transitions the caller is authorized to make. Publishing transitions are gated by
// the collection's `publish` rule (#23) when it declares one, else by update.
func (s *Server) adminLifecycleFor(r *http.Request, cd schema.CollectionDef, rec store.Record) *adminLifecycle {
	id, _ := rec["id"].(string)
	canPublish := s.adminCanRule(r, cd.Name, id, publishOrUpdateRule(cd))
	canUpdate := s.adminCanRecord(r, cd.Name, id, schema.ActionUpdate)
	canDelete := s.adminCanRecord(r, cd.Name, id, schema.ActionDelete)

	lc := &adminLifecycle{Status: adminRowStatus(cd, rec)}

	if cd.SoftDelete && isSet(rec[schema.LifecycleDeletedAt]) {
		lc.Help = "In the trash — hidden from your site. Restore it to bring it back."
		if canUpdate {
			lc.Actions = append(lc.Actions, adminAction{Label: "Restore", Verb: "restore", Style: "primary"})
		}
		lc.HardDelete = canDelete
		return lc
	}

	if cd.Publishing {
		status, _ := rec[schema.LifecycleStatus].(string)
		switch {
		case status == schema.StatusPublished && publishedAtReached(rec):
			lc.Help = "Live on your website."
			if canPublish {
				lc.Actions = append(lc.Actions, adminAction{Label: "Unpublish", Verb: "unpublish", Style: "default"})
			}
		case status == schema.StatusPublished:
			lc.Help = "Scheduled to go live automatically — not visible yet."
			if canPublish {
				lc.Actions = append(lc.Actions, adminAction{Label: "Publish now", Verb: "publish", Style: "primary"})
				lc.Actions = append(lc.Actions, adminAction{Label: "Unpublish", Verb: "unpublish", Style: "default"})
			}
		case status == schema.StatusArchived:
			lc.Help = "Archived — kept, but not shown on your site."
			if canPublish {
				lc.Actions = append(lc.Actions, adminAction{Label: "Publish", Verb: "publish", Style: "primary"})
			}
		default: // draft
			lc.Help = "Draft — not visible on your site yet."
			if canPublish {
				lc.Actions = append(lc.Actions, adminAction{Label: "Publish", Verb: "publish", Style: "primary"})
			}
		}
		if canPublish && status != schema.StatusArchived {
			lc.Actions = append(lc.Actions, adminAction{Label: "Archive", Verb: "archive", Style: "default"})
		}
	}

	if cd.SoftDelete {
		if canDelete {
			lc.Actions = append(lc.Actions, adminAction{Label: "Move to trash", Verb: "trash", Style: "danger",
				Confirm: "Move this to the trash? You can restore it later."})
		}
	} else {
		lc.HardDelete = canDelete
	}
	return lc
}

// publishOrUpdateRule returns the rule that gates a publishing transition: the
// collection's `publish` rule when declared (#23), otherwise its update rule.
func publishOrUpdateRule(cd schema.CollectionDef) schema.Rule {
	if pr, ok := cd.PublishRule(); ok {
		return pr
	}
	return cd.AccessRule(schema.ActionUpdate)
}

// ── transition handlers ────────────────────────────────────────────────────────

func (s *Server) adminPublish(w http.ResponseWriter, r *http.Request) {
	s.adminTransition(w, r, "publish", "Published", func(id string) store.Record {
		return store.Record{"id": id, schema.LifecycleStatus: schema.StatusPublished, schema.LifecyclePublishedAt: nowUTC()}
	})
}

func (s *Server) adminUnpublish(w http.ResponseWriter, r *http.Request) {
	s.adminTransition(w, r, "unpublish", "Moved to draft", func(id string) store.Record {
		return store.Record{"id": id, schema.LifecycleStatus: schema.StatusDraft, schema.LifecyclePublishedAt: nil}
	})
}

func (s *Server) adminArchive(w http.ResponseWriter, r *http.Request) {
	s.adminTransition(w, r, "archive", "Archived", func(id string) store.Record {
		return store.Record{"id": id, schema.LifecycleStatus: schema.StatusArchived}
	})
}

func (s *Server) adminTrash(w http.ResponseWriter, r *http.Request) {
	s.adminTransition(w, r, "delete", "Moved to trash", func(id string) store.Record {
		return store.Record{"id": id, schema.LifecycleDeletedAt: nowUTC()}
	})
}

func (s *Server) adminRestore(w http.ResponseWriter, r *http.Request) {
	s.adminTransition(w, r, "restore", "Restored", func(id string) store.Record {
		return store.Record{"id": id, schema.LifecycleDeletedAt: nil}
	})
}

// adminTransition runs a managed lifecycle write the same way the API transition
// endpoints do: CSRF, hidden-record 404, authorize (publish rule for publishing
// operations, update for the rest), then updateAndRevise so revisions, events and
// scheduling stay consistent. It redirects back to the edit page with a flash.
func (s *Server) adminTransition(w http.ResponseWriter, r *http.Request, op, flash string, mutate func(id string) store.Record) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	// A record the caller may not preview does not exist for them (ADR-0023).
	if s.writeHidden(r.Context(), cd.Name, id) {
		s.adminError(w, r, "Record not found.")
		return
	}
	rule := cd.AccessRule(schema.ActionUpdate)
	switch op {
	case "publish", "unpublish", "archive":
		rule = publishOrUpdateRule(cd)
	case "delete":
		rule = cd.AccessRule(schema.ActionDelete)
	}
	if !s.adminCanRule(r, cd.Name, id, rule) {
		s.adminForbidden(w, r)
		return
	}
	if _, err := s.updateAndRevise(r.Context(), cd.Name, mutate(id), op, nil); err != nil {
		s.adminError(w, r, "Could not complete that action.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/c/"+cd.Name+"/"+id+"?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}
