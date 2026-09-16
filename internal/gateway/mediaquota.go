package gateway

import (
	"context"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Per-principal media storage quota (issue #31). A principal's total is the sum of
// the sizes of the _media rows they created; a new upload is refused with 413 when
// it would push that total over the principal's quota. created_by and size already
// live on _media, so the total is one indexed SUM — no separate counter.

// MediaQuotaOptions is the resolved quota policy: a default byte cap and optional
// per-role caps. A negative cap (-1) means unlimited. The most permissive matching
// cap wins, so a role can only raise a principal's limit, never lower it.
type MediaQuotaOptions struct {
	Default int64            // bytes; -1 = unlimited
	Roles   map[string]int64 // role -> bytes; -1 = unlimited
}

// quotaFor returns the effective byte cap for a principal holding these roles, or
// -1 for unlimited. Unlimited anywhere applicable wins; otherwise the largest cap.
func (q *MediaQuotaOptions) quotaFor(roles []string) int64 {
	if q.Default < 0 {
		return -1
	}
	limit := q.Default
	for _, role := range roles {
		rq, ok := q.Roles[role]
		if !ok {
			continue
		}
		if rq < 0 {
			return -1
		}
		if rq > limit {
			limit = rq
		}
	}
	return limit
}

// mediaUsage returns the total bytes of media a principal has uploaded (SUM of size
// over their created_by rows). Zero when they have none.
func (s *Server) mediaUsage(ctx context.Context, principalID string) (int64, error) {
	res, err := s.db.Aggregate(ctx, store.AggregateQuery{
		Collection: schema.MediaCollection,
		Metric:     store.Sum,
		Field:      schema.MediaSize,
		Filters:    []store.Filter{{Field: createdByField, Operator: store.Eq, Value: principalID}},
	})
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, nil
	}
	return int64(res[0].Value), nil
}
