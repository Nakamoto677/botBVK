package postgres

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
)

var _ domain.WorkflowStore = (*Store)(nil)

func (s *Store) SaveAuxiliary(ctx context.Context, m domain.AuxiliaryMessage) error {
	if m.MessageID <= 0 || m.UserID == 0 || m.ChatID == 0 || m.Scenario == "" {
		return domain.ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bvk_auxiliary_messages(user_id,chat_id,topic_id,scenario,message_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, m.UserID, m.ChatID, m.TopicID, m.Scenario, m.MessageID)
	return err
}
func (s *Store) Auxiliary(ctx context.Context, user, chat, topic int64, scenario string) ([]domain.AuxiliaryMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id,chat_id,topic_id,scenario,message_id FROM bvk_auxiliary_messages WHERE user_id=$1 AND chat_id=$2 AND topic_id=$3 AND scenario=$4 ORDER BY message_id`, user, chat, topic, scenario)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AuxiliaryMessage{}
	for rows.Next() {
		var m domain.AuxiliaryMessage
		if err = rows.Scan(&m.UserID, &m.ChatID, &m.TopicID, &m.Scenario, &m.MessageID); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
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

// CleanupSettled prevents removing a topic while an accepted transcript or
// attachment still needs durable processing, including a failed operation.
func (s *Store) CleanupSettled(ctx context.Context, ticketID int64) (bool, error) {
	var unsettled bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bvk_updates WHERE NOT finished) OR EXISTS(SELECT 1 FROM bvk_outbox WHERE ticket_id=$1 AND state IN ('pending','processing','failed') AND kind IN ('archive_message','client_message','team_message','create_topic','reopen_topic'))`, ticketID).Scan(&unsettled)
	return !unsettled, err
}

// CleanupRetry retries archival failures only. Unsupported media and oversized
// attachments remain blocked and visible to administrators.
func (s *Store) CleanupRetry(ctx context.Context, ticketID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE bvk_outbox o SET state='pending',attempts=0,available_at=now(),lease_until=NULL WHERE o.ticket_id=$1 AND o.kind='archive_message' AND o.state='failed' AND EXISTS(SELECT 1 FROM bvk_messages m WHERE m.ticket_id=o.ticket_id AND m.id=COALESCE(o.payload->>'ID',o.payload->>'message_id')::bigint AND COALESCE(m.payload->>'ArchivePath','')='' AND COALESCE((m.payload->>'FileSize')::bigint,0)<=20971520) AND NOT EXISTS(SELECT 1 FROM bvk_events e WHERE e.ticket_id=o.ticket_id AND e.payload->>'Kind'='media_edit_unsupported')`, ticketID)
	return err
}

func (s *Store) CleanupProblem(ctx context.Context, ticketID int64) (string, error) {
	var kind, state, lastError string
	err := s.pool.QueryRow(ctx, `SELECT kind,state,last_error FROM bvk_outbox WHERE ticket_id=$1 AND state IN ('pending','processing','failed') AND kind IN ('archive_message','client_message','team_message','create_topic','reopen_topic') ORDER BY CASE WHEN state='failed' THEN 0 ELSE 1 END,id LIMIT 1`, ticketID).Scan(&kind, &state, &lastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "обработка уже принятых входящих сообщений ещё не завершена", nil
		}
		return "", mapError(err)
	}
	// Return a bounded operator-facing description, never raw errors that might
	// contain URLs, filesystem credentials or PostgreSQL connection secrets.
	label := map[string]string{"archive_message": "сохранение вложения", "client_message": "доставка клиенту", "team_message": "доставка инженерам", "create_topic": "создание топика", "reopen_topic": "подготовка топика"}[kind]
	if state == "failed" {
		lower := strings.ToLower(lastError)
		switch {
		case strings.Contains(lower, "20 mb") || strings.Contains(lower, "20mb") || strings.Contains(lower, "too big") || strings.Contains(lower, "too large"):
			return label + ": файл превышает лимит независимого архива", nil
		case strings.Contains(lower, "forbidden") || strings.Contains(lower, "blocked"):
			return label + ": Telegram запретил доступ или бот заблокирован", nil
		case strings.Contains(lower, "not found"):
			return label + ": исходный файл или получатель недоступен", nil
		default:
			return label + ": ошибка; проверьте доступ и журнал, затем повторите операцию", nil
		}
	}
	return label + ": обработка ещё не завершена", nil
}

// CleanupFence shares a lock with inbox acceptance. Updates committed before
// this point must be processed before destructive deletion can proceed.
func (s *Store) CleanupFence(ctx context.Context, tx domain.Tx, ticketID int64) error {
	raw, ok := tx.(*transaction)
	if !ok {
		return domain.ErrInvalid
	}
	if _, err := raw.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(731903)"); err != nil {
		return err
	}
	var pending bool
	if err := raw.tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM bvk_updates WHERE NOT finished)").Scan(&pending); err != nil {
		return err
	}
	if pending {
		return domain.ErrConflict
	}
	return nil
}
