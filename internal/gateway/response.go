package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/blazing-Gael/dcms/internal/store"
)

// apiError is the body of an error response.
type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeData writes a single-record success envelope: {"data": ..., "meta": {}}.
func writeData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"data": data, "meta": map[string]any{}})
}

// writeList writes a list success envelope with pagination metadata.
func writeList(w http.ResponseWriter, page store.Page, limit int) {
	data := page.Data
	if data == nil {
		data = []store.Record{} // encode an empty list as [], never null
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{
			"total":       page.Total,
			"limit":       limit,
			"next_cursor": page.NextCursor,
		},
	})
}

// writeRecord shapes a single record to the schema's declared types (always)
// and, when strict response validation is enabled, verifies it conforms before
// sending. A validation failure means the server produced non-conforming data,
// so it is logged in full and surfaced as a 500 — never leaked to the client.
func (s *Server) writeRecord(w http.ResponseWriter, r *http.Request, status int, collection string, rec store.Record, expand []string) {
	cd := s.collections[collection]
	cd.CoerceResponse(rec)
	if s.opts.ValidateResponses {
		if errs := cd.ValidateResponse(rec); errs != nil {
			s.logger.Error("response contract violation",
				"collection", collection, "id", rec["id"], "fields", errs, "path", r.URL.Path)
			writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
			return
		}
	}
	if len(expand) > 0 {
		if err := s.expandRecord(r.Context(), collection, rec, expand); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeData(w, status, rec)
}

// writeRecords does the same for a list response: every record is shaped, and
// (when enabled) validated before any bytes are written.
func (s *Server) writeRecords(w http.ResponseWriter, r *http.Request, collection string, page store.Page, limit int, expand []string) {
	cd := s.collections[collection]
	for _, rec := range page.Data {
		cd.CoerceResponse(rec)
	}
	if s.opts.ValidateResponses {
		for _, rec := range page.Data {
			if errs := cd.ValidateResponse(rec); errs != nil {
				s.logger.Error("response contract violation",
					"collection", collection, "id", rec["id"], "fields", errs, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
				return
			}
		}
	}
	if len(expand) > 0 {
		if err := s.expandListRecords(r.Context(), collection, page.Data, expand); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeList(w, page, limit)
}

// writeError writes an error envelope: {"error": {...}}.
func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]any{"error": e})
}

// writeStoreError maps a store-layer error to the right HTTP status + error code.
// Internal errors are logged in full but never exposed to the client.
func writeStoreError(w http.ResponseWriter, logger *slog.Logger, r *http.Request, err error) {
	var ve *store.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, apiError{
			Code: "VALIDATION_ERROR", Message: "validation failed", Fields: ve.Fields,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "record not found"})
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "resource already exists"})
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
	case errors.Is(err, store.ErrIntegrity):
		// A referential integrity violation on a write is an invariant breach:
		// the validation layer should have rejected the reference first
		// (ADR-0010). Log it as a real problem; never surface it as a friendly
		// 422, which would hide the bug. (Delete's RESTRICT 409 is handled in the
		// delete handler before reaching here.)
		logger.Error("referential integrity violation on write (validation layer gap?)",
			"err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
	default:
		// Never leak internal details. Log the full error, return a generic one.
		logger.Error("internal error", "err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
	}
}
