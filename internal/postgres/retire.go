package postgres

import (
	"context"
	"log/slog"
	"time"
)

// unfinishedRetirementAge is how long an untied failed operation is kept before it is
// retired. An operation with no ticket is a screen or a notification whose target can
// no longer be recovered from the stored payload, so after this window it is only
// noise in the readiness probe. The window exists so an operator still has a chance to
// see it and investigate a fresh incident.
const unfinishedRetirementAge = 15 * time.Minute

// RetireFinishedFailures clears failed operations that are provably pointless to keep.
//
// A failed operation is terminal: the worker never selects it again, and /ready reports
// not-ready while any failed row exists. Only a human could clear them, so one dead row
// used to pin the bot to "unavailable" forever.
//
// Only what cannot be retried usefully is retired:
//
//   - the operation belongs to a ticket that is already closed or cancelled, so there
//     is no transcript left to deliver;
//   - the operation has no reachable ticket, which is how a queued screen looks, and it
//     is older than the retirement window.
//
// A failure on a live ticket is deliberately kept: there the client or the engineers
// are still waiting, the problem is real, and hiding it would remove the only signal.
// The reason is kept in last_error and each retirement is logged.
func (s *Store) RetireFinishedFailures(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id, o.kind, o.ticket_id
		FROM bvk_outbox o
		WHERE o.state='failed' AND (
			EXISTS (SELECT 1 FROM bvk_tickets t
			        WHERE t.id=o.ticket_id AND t.status NOT IN ('new','in_progress','waiting_client'))
			OR (o.ticket_id=0 AND o.available_at < now() - $1::interval)
		)
		ORDER BY o.id`, unfinishedRetirementAge.String())
	if err != nil {
		return 0, err
	}
	type retired struct {
		id     int64
		kind   string
		ticket int64
	}
	var candidates []retired
	for rows.Next() {
		var r retired
		if err = rows.Scan(&r.id, &r.kind, &r.ticket); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	// Retire by id so a row that became retryable meanwhile cannot be swallowed.
	ids := make([]int64, 0, len(candidates))
	for _, r := range candidates {
		ids = append(ids, r.id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE bvk_outbox
		SET state='done', lease_until=NULL,
		    last_error = left('снято автоматически: '||last_error, 500)
		WHERE id = ANY($1::bigint[]) AND state='failed'`, ids)
	if err != nil {
		return 0, err
	}
	count := int(tag.RowsAffected())
	for _, r := range candidates {
		slog.Info("retired a failed operation that cannot be retried",
			"operation", r.id, "kind", r.kind, "ticket", r.ticket)
	}
	return count, nil
}
