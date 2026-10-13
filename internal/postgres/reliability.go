package postgres

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"time"
)

func (s *Store) RetryUpdate(ctx context.Context, id int64, reason string, delay time.Duration, quarantine bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE bvk_updates SET attempts=attempts+1,last_error=$2,available_at=now()+($3::bigint * interval '1 microsecond'),quarantined=$4 WHERE id=$1 AND NOT finished`, id, reason, delay.Microseconds(), quarantine)
	return err
}
func (s *Store) LoadDraft(ctx context.Context, id int64) (domain.Draft, error) {
	return loadDraft(ctx, s.pool, id, false)
}
func (t *transaction) LoadDraft(ctx context.Context, id int64) (domain.Draft, error) {
	return loadDraft(ctx, t.tx, id, true)
}
func loadDraft(ctx context.Context, q querier, id int64, lock bool) (domain.Draft, error) {
	var d domain.Draft
	var raw []byte
	sql := "SELECT department,body,state FROM bvk_drafts WHERE user_id=$1"
	if lock {
		sql += " FOR UPDATE"
	}
	err := q.QueryRow(ctx, sql, id).Scan(&d.Department, &d.Text, &raw)
	if err != nil {
		return d, mapError(err)
	}
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &d)
	}
	return d, err
}
func (s *Store) PutDraft(ctx context.Context, id int64, d domain.Draft) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO bvk_drafts(user_id,department,body,state) VALUES($1,$2,$3,$4) ON CONFLICT(user_id) DO UPDATE SET department=EXCLUDED.department,body=EXCLUDED.body,state=EXCLUDED.state,updated_at=now()`, id, d.Department, d.Text, raw)
	return mapError(err)
}
func (t *transaction) CompleteDraft(ctx context.Context, id int64, generation string, ticket int64) error {
	tag, err := t.tx.Exec(ctx, `UPDATE bvk_drafts SET state=jsonb_set(jsonb_set(state,'{Step}','"submitted"'),'{TicketID}',to_jsonb($3::bigint)) WHERE user_id=$1 AND state->>'Generation'=$2 AND state->>'Step'='confirm'`, id, generation, ticket)
	if err == nil && tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return err
}

func (s *Store) SendPart(ctx context.Context, key string, part int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT message_id FROM bvk_send_parts WHERE key=$1 AND part=$2", key, part).Scan(&id)
	return id, mapError(err)
}
func (s *Store) SaveSendPart(ctx context.Context, key string, part int, id int64) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO bvk_send_parts(key,part,message_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", key, part, id)
	return err
}
