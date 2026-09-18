package gateway

import (
	"context"
	"fmt"
	"net/http"

	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Extension hooks (ADR-0031): opt-in, in-process business logic that runs on the
// write lifecycle of the schema-generated endpoints. A hook is trusted Go code the
// operator compiles in; it runs inside the write transaction, so a rejection rolls
// the whole write back and a hook's own writes commit atomically with it.
//
// This is the in-process transport. The interface a hook is handed (HookContext +
// the narrow HookStore) is deliberately small — least privilege today, and the seam
// a future out-of-process/WASM transport would serialize (ADR-0031 §4). No wire
// format or sandbox is built here.

// HookEvent names a point in a write's lifecycle. A Before* hook runs before the
// row is written (and may mutate the record or reject); an After* hook runs after
// it is written and its side effects (links, revision, event) are captured.
type HookEvent string

const (
	BeforeCreate HookEvent = "before_create"
	AfterCreate  HookEvent = "after_create"
	BeforeUpdate HookEvent = "before_update"
	AfterUpdate  HookEvent = "after_update"
	BeforeDelete HookEvent = "before_delete"
	AfterDelete  HookEvent = "after_delete"
)

// HookStore is the narrow slice of the store a hook may use: scoped reads and
// writes that enroll in the current transaction. It is intentionally not the full
// store.DB — no raw SQL, no migrations — so a hook has least privilege and the
// contract stays small (ADR-0031 §4). The transaction handle satisfies it.
type HookStore interface {
	FindOne(ctx context.Context, collection, id string) (store.Record, error)
	Find(ctx context.Context, q store.Query) (store.Page, error)
	Create(ctx context.Context, in store.WriteInput) (store.Record, error)
	Update(ctx context.Context, in store.WriteInput) (store.Record, error)
	Delete(ctx context.Context, collection, id string) error
}

// HookContext is what a hook receives about the write it is running on. The
// Principal is the *verified* identity (read-only); a hook never sets
// created_by/updated_by — the adapter still stamps those from the verified
// identity (ADR-0016 audit invariant). Store enrolls in the write's transaction.
type HookContext struct {
	Principal  auth.Principal
	Collection string
	Event      HookEvent
	Store      HookStore
}

// WriteHook is one registered hook. For a Before* event it receives the record
// about to be written and returns the record to write (possibly mutated) or an
// error to reject the whole write. For an After* event it receives the written
// record; its returned record is ignored (the row is already written) — only a
// returned error, which rolls the transaction back, is honored.
type WriteHook func(ctx context.Context, hc HookContext, rec store.Record) (store.Record, error)

// HookError lets a hook choose the HTTP status of a rejection. A Before* hook that
// returns one renders as that status (e.g. 422 for a business-rule violation, 409
// for a conflict); any other error from a hook is treated as an unexpected server
// fault (500), since hooks are trusted code. Zero Status defaults to 422.
type HookError struct {
	Status  int
	Code    string
	Message string
}

func (e *HookError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "hook rejected the write"
}

// status/code/message normalize a HookError for rendering, applying the defaults.
func (e *HookError) status() int {
	if e.Status != 0 {
		return e.Status
	}
	return http.StatusUnprocessableEntity
}

func (e *HookError) apiErr() apiError {
	code := e.Code
	if code == "" {
		code = "HOOK_REJECTED"
	}
	msg := e.Message
	if msg == "" {
		msg = "the write was rejected by a business rule"
	}
	return apiError{Code: code, Message: msg}
}

// HookRegistry holds the hooks registered for each (collection, event). Build one
// with NewHookRegistry, add hooks with On, and pass it via gateway.Options.Hooks.
// It is read-only once the server is constructed.
type HookRegistry struct {
	byCollection map[string]map[HookEvent][]WriteHook
}

// NewHookRegistry returns an empty registry.
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{byCollection: map[string]map[HookEvent][]WriteHook{}}
}

// On registers fn to run on event for collection. Hooks fire in registration
// order. It returns the registry so calls can chain.
func (h *HookRegistry) On(collection string, event HookEvent, fn WriteHook) *HookRegistry {
	if h.byCollection[collection] == nil {
		h.byCollection[collection] = map[HookEvent][]WriteHook{}
	}
	h.byCollection[collection][event] = append(h.byCollection[collection][event], fn)
	return h
}

// hooksFor returns the hooks registered for a (collection, event), or nil.
func (s *Server) hooksFor(collection string, event HookEvent) []WriteHook {
	if s.opts.Hooks == nil {
		return nil
	}
	return s.opts.Hooks.byCollection[collection][event]
}

// hasWriteHooks reports whether any create/update/delete hook is registered for a
// collection, so the write fast-paths know to take the transaction (a hook must run
// inside it). Cheap: a couple of map lookups, only when hooks are configured at all.
func (s *Server) hasWriteHooks(collection string) bool {
	if s.opts.Hooks == nil {
		return false
	}
	return len(s.opts.Hooks.byCollection[collection]) > 0
}

// beforeWrite runs the Before* hooks for an event, threading the record through
// each (a hook may mutate it). It returns the final record to write, or the first
// hook's error (which rolls the transaction back). tx is the write's transaction,
// handed to hooks as the narrow HookStore so their side-effect writes are atomic.
func (s *Server) beforeWrite(ctx context.Context, tx store.DB, collection string, event HookEvent, rec store.Record) (store.Record, error) {
	hooks := s.hooksFor(collection, event)
	if len(hooks) == 0 {
		return rec, nil
	}
	hc := HookContext{Principal: principalFromContext(ctx), Collection: collection, Event: event, Store: tx}
	for i, fn := range hooks {
		next, err := fn(ctx, hc, rec)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, fmt.Errorf("hook %d for %s.%s returned a nil record", i, collection, event)
		}
		rec = next
	}
	return rec, nil
}

// afterWrite runs the After* hooks for an event on the written record. A returned
// record is ignored (the row is already written); only an error is honored, which
// rolls the transaction back.
func (s *Server) afterWrite(ctx context.Context, tx store.DB, collection string, event HookEvent, rec store.Record) error {
	hooks := s.hooksFor(collection, event)
	if len(hooks) == 0 {
		return nil
	}
	hc := HookContext{Principal: principalFromContext(ctx), Collection: collection, Event: event, Store: tx}
	for _, fn := range hooks {
		if _, err := fn(ctx, hc, rec); err != nil {
			return err
		}
	}
	return nil
}

// beforeWriteEvent / afterWriteEvent map a write operation ("create"/"update") to
// its lifecycle events, so the shared write path can run the right hooks.
func beforeWriteEvent(operation string) (HookEvent, bool) {
	switch operation {
	case "create":
		return BeforeCreate, true
	case "update":
		return BeforeUpdate, true
	default:
		return "", false
	}
}

func afterWriteEvent(operation string) (HookEvent, bool) {
	switch operation {
	case "create":
		return AfterCreate, true
	case "update":
		return AfterUpdate, true
	default:
		return "", false
	}
}
