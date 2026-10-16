package postgres

import (
	"bot-bvk/internal/domain"
	"context"
)

var _ domain.QuarantineStore = (*Store)(nil)

// QuarantinedUpdate describes an accepted update that exhausted its attempts.
// The row stays unfinished on purpose: CleanupSettled and CleanupFence rely on
// "NOT finished" to refuse destructive topic deletion while an accepted message
// is still unclassified. ReleaseQuarantine is the only supported way out.
//
// A quarantined row carries no label saying which actor it belongs to, so the
// identity is read from the stored payload the same way the SQL key builder does.
// Telegram update IDs order oldest first.

// QuarantinedUpdates lists quarantined rows oldest first.
func (s *Store) QuarantinedUpdates(ctx context.Context) ([]domain.QuarantinedUpdate, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,`+actorFromPayloadSQL+`,`+chatFromPayloadSQL+`,attempts,last_error FROM bvk_updates WHERE NOT finished AND quarantined ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.QuarantinedUpdate{}
	for rows.Next() {
		var u domain.QuarantinedUpdate
		if err = rows.Scan(&u.ID, &u.ActorID, &u.ChatID, &u.Attempts, &u.LastError); err != nil {
			return nil, err
		}
		result = append(result, u)
	}
	return result, rows.Err()
}

// QuarantineCount returns the number of quarantined updates for health checks.
func (s *Store) QuarantineCount(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM bvk_updates WHERE NOT finished AND quarantined`).Scan(&count)
	return count, err
}

// ReleaseQuarantine returns one quarantined update to the pending lane. Attempts
// are not reset, so the update keeps its history and is quarantined again on the
// next attempt if the cause is permanent. The row is marked finished only when it
// is genuinely processed, which keeps the archive fence for every other topic.
func (s *Store) ReleaseQuarantine(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE bvk_updates SET quarantined=false,available_at=now() WHERE id=$1 AND NOT finished AND quarantined`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
