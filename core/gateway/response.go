package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/blazing-Gael/dcms/core/store"
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
	default:
		// Never leak internal details. Log the full error, return a generic one.
		logger.Error("internal error", "err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
	}
}
