package postgres

import (
	"context"

	"bot-bvk/internal/domain"
)

var _ domain.ClientTopicStore = (*Store)(nil)
var _ domain.ClientTopicTx = (*transaction)(nil)

func (s *Store) ClientNumber(ctx context.Context, clientID int64) (int64, error) {
	var number int64
	err := s.pool.QueryRow(ctx, "SELECT number FROM bvk_client_numbers WHERE client_id=$1", clientID).Scan(&number)
	return number, mapError(err)
}

func (s *Store) ClientTopic(ctx context.Context, clientID int64, department domain.Department) (domain.ClientTopic, error) {
	var topic domain.ClientTopic
	err := s.pool.QueryRow(ctx, "SELECT client_id,department,chat_id,topic_id FROM bvk_client_topics WHERE client_id=$1 AND department=$2", clientID, department).Scan(&topic.ClientID, &topic.Department, &topic.ChatID, &topic.TopicID)
	return topic, mapError(err)
}

func (t *transaction) BindClientTopic(ctx context.Context, topic domain.ClientTopic) error {
	_, err := t.tx.Exec(ctx, "INSERT INTO bvk_client_topics(client_id,department,chat_id,topic_id) VALUES($1,$2,$3,$4)", topic.ClientID, topic.Department, topic.ChatID, topic.TopicID)
	return mapError(err)
}

func (t *transaction) ReplaceClientTopic(ctx context.Context, topic domain.ClientTopic, previousTopicID int64) error {
	tag, err := t.tx.Exec(ctx, "UPDATE bvk_client_topics SET chat_id=$3,topic_id=$4 WHERE client_id=$1 AND department=$2 AND topic_id=$5", topic.ClientID, topic.Department, topic.ChatID, topic.TopicID, previousTopicID)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func (s *Store) PreviousTopicTicket(ctx context.Context, clientID int64, department domain.Department, beforeID int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, s.pool, `SELECT p.payload FROM bvk_tickets c
		JOIN bvk_tickets p ON p.client_id=c.client_id
		 AND p.department=c.department AND p.topic_chat_id=c.topic_chat_id
		 AND p.topic_id=c.topic_id AND p.id<c.id
		WHERE c.id=$3 AND c.client_id=$1 AND c.department=$2 AND c.topic_id>0
		ORDER BY p.id DESC LIMIT 1`, clientID, department, beforeID)
}

func (s *Store) ActiveClientTopicTicket(ctx context.Context, clientID int64, department domain.Department, excludingID int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, s.pool, "SELECT payload FROM bvk_tickets WHERE client_id=$1 AND department=$2 AND id<>$3 AND status IN ('in_progress','waiting_client') LIMIT 1", clientID, department, excludingID)
}
