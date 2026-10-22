package postgres

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
)

var _ domain.WorkflowStore = (*Store)(nil)

func (s *Store) SaveAuxiliary(ctx context.Context, m domain.AuxiliaryMessage) error {
	if m.MessageID <= 0 || m.UserID == 0 || m.ChatID == 0 || m.Scenario == "" {
		return domain.ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bvk_auxiliary_messages(user_id,chat_id,topic_id,scenario,message_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, m.UserID, m.ChatID, m.TopicID, m.Scenario, m.MessageID)
	return err
}
func (s *Store) QueueAuxiliaryCleanup(ctx context.Context, user, chat, topic int64, scenario string) error {
	return s.Transact(ctx, func(tx domain.Tx) error {
		// DELETE RETURNING and the outbox insert are one transaction. Concurrent
		// callers cannot enqueue the same auxiliary message twice.
		rows, err := tx.(*transaction).tx.Query(ctx, `DELETE FROM bvk_auxiliary_messages WHERE user_id=$1 AND chat_id=$2 AND topic_id=$3 AND scenario=$4 RETURNING message_id`, user, chat, topic, scenario)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			raw, _ := json.Marshal(map[string]int64{"chat_id": chat, "message_id": id})
			if err = tx.Enqueue(ctx, domain.Operation{Kind: "delete_aux_message", Payload: raw}); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) SaveWorkflow(ctx context.Context, user, chat, topic int64, scenario string, payload []byte) error {
	if !json.Valid(payload) || user == 0 || chat == 0 || scenario == "" {
		return domain.ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bvk_workflows(user_id,chat_id,topic_id,scenario,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id,chat_id,topic_id,scenario) DO UPDATE SET payload=EXCLUDED.payload`, user, chat, topic, scenario, payload)
	return err
}
func (s *Store) Workflow(ctx context.Context, user, chat, topic int64, scenario string) ([]byte, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM bvk_workflows WHERE user_id=$1 AND chat_id=$2 AND topic_id=$3 AND scenario=$4`, user, chat, topic, scenario).Scan(&raw)
	return raw, mapError(err)
}
func (s *Store) DeleteWorkflow(ctx context.Context, user, chat, topic int64, scenario string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM bvk_workflows WHERE user_id=$1 AND chat_id=$2 AND topic_id=$3 AND scenario=$4`, user, chat, topic, scenario)
	return err
}
