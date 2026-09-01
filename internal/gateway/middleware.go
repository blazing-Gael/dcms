package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger logs one structured line per request: method, path, status,
// duration, and the request id (set by chi's RequestID middleware). It wraps the
// ResponseWriter to capture the status code, and logs in a defer so the line is
// emitted even if a downstream handler panics.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			s.logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		}()

		next.ServeHTTP(ww, r)
	})
}

// limitBody caps the request body at the configured size (defaultMaxBodyBytes
// when unset). It wraps r.Body in an http.MaxBytesReader, so a body over the cap
// fails at decode time with an *http.MaxBytesError, which decodeBody surfaces as
// a 413. This bounds the memory a single JSON request can force us to buffer.
func (s *Server) limitBody(next http.Handler) http.Handler {
	limit := s.maxBodyBytes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// withTimeout gives each request a deadline (defaultRequestTimeout when unset;
// disabled when the configured timeout is negative). It is cooperative: the store
// honors ctx cancellation at query boundaries, so an over-budget request's DB
// call returns context.DeadlineExceeded, which writeStoreError maps to a 504.
func (s *Server) withTimeout(next http.Handler) http.Handler {
	d := s.requestTimeout()
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverer turns a panic in any handler into a logged 500 with our standard
// error envelope, instead of crashing the connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "err", rec, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
