package postgres

import (
	"context"
	"errors"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
)

// CompleteCompanyBinding runs under the same transaction and user lock as the
// profile update. Historical tickets keep their own company_id.
func (t *transaction) CompleteCompanyBinding(ctx context.Context, userID, previousCompanyID, companyID int64) error {
	var active bool
	if err := t.tx.QueryRow(ctx, "SELECT registration_active FROM bvk_companies WHERE id=$1 FOR SHARE", companyID).Scan(&active); err != nil {
		return mapError(err)
	}
	if !active {
		return domain.ErrInvalid
	}
	if _, err := t.tx.Exec(ctx, "DELETE FROM bvk_drafts WHERE user_id=$1", userID); err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, "UPDATE bvk_access SET status='superseded' WHERE user_id=$1 AND status='pending'", userID); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx, "INSERT INTO bvk_access(user_id,company_id,status,method,previous_company_id) VALUES($1,$2,'approved','company_code',NULLIF($3,0))", userID, companyID, previousCompanyID)
	return mapError(err)
}

// RecordCompanyCodeAttempt applies a durable five-attempt limit per Telegram
// account in a fifteen-minute window. A valid code clears the counter only
// when the account is not already cooling down.
func (s *Store) RecordCompanyCodeAttempt(ctx context.Context, userID int64, valid bool) (bool, error) {
	blocked := false
	err := s.Transact(ctx, func(t domain.Tx) error {
		tx := t.(*transaction).tx
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(-$1::bigint)", userID); err != nil {
			return err
		}
		var attempts int
		var started time.Time
		var until *time.Time
		err := tx.QueryRow(ctx, "SELECT attempts,window_started_at,blocked_until FROM bvk_company_code_attempts WHERE user_id=$1 FOR UPDATE", userID).Scan(&attempts, &started, &until)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := time.Now().UTC()
		if until != nil && now.Before(*until) {
			blocked = true
			return nil
		}
		if valid {
			_, err = tx.Exec(ctx, "DELETE FROM bvk_company_code_attempts WHERE user_id=$1", userID)
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) || now.Sub(started) >= 15*time.Minute {
			attempts, started = 0, now
		}
		attempts++
		var nextUntil *time.Time
		if attempts >= 5 {
			v := now.Add(15 * time.Minute)
			nextUntil = &v
			blocked = true
		}
		_, err = tx.Exec(ctx, "INSERT INTO bvk_company_code_attempts(user_id,attempts,window_started_at,blocked_until) VALUES($1,$2,$3,$4) ON CONFLICT(user_id) DO UPDATE SET attempts=EXCLUDED.attempts,window_started_at=EXCLUDED.window_started_at,blocked_until=EXCLUDED.blocked_until", userID, attempts, started, nextUntil)
		return err
	})
	return blocked, err
}

// FinishSensitiveUpdate keeps the update id for deduplication and ordering,
// while removing a submitted company code from the durable inbox.
func (s *Store) FinishSensitiveUpdate(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE bvk_updates SET finished=true,payload=jsonb_set(payload,'{message,text}','"[company code redacted]"'::jsonb,false) WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}
