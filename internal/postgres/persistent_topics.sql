-- Run with the old workers stopped. Never allow an old queued delete to remove
-- a conversation after this schema starts sharing topics between tickets.
UPDATE bvk_outbox SET state='done', lease_until=NULL,
 last_error='Отменено: история клиента хранится в постоянном топике'
WHERE kind='delete_topic' AND state IN ('pending','processing','failed');

UPDATE bvk_outbox o SET conversation_id=-(SELECT client_id FROM bvk_tickets WHERE id=o.ticket_id)
WHERE kind IN ('create_topic','reopen_topic','close_topic') AND state IN ('pending','processing');

CREATE TABLE IF NOT EXISTS bvk_client_numbers (
 number bigserial UNIQUE NOT NULL,
 client_id bigint PRIMARY KEY REFERENCES bvk_users(id)
);
INSERT INTO bvk_client_numbers(client_id)
SELECT DISTINCT client_id FROM bvk_tickets ORDER BY client_id ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS bvk_client_topics (
 client_id bigint NOT NULL REFERENCES bvk_users(id),
 department text NOT NULL CHECK (department IN ('it','1c','installation','field')),
 chat_id bigint NOT NULL,
 topic_id bigint NOT NULL,
 PRIMARY KEY(client_id,department),
 UNIQUE(chat_id,topic_id),
 CHECK (chat_id<>0 AND topic_id>0)
);
INSERT INTO bvk_client_topics(client_id,department,chat_id,topic_id)
SELECT DISTINCT ON (client_id,department) client_id,department,topic_chat_id,topic_id
FROM bvk_tickets WHERE topic_id>0 ORDER BY client_id,department,id DESC
ON CONFLICT DO NOTHING;

DROP INDEX IF EXISTS bvk_topic;
CREATE INDEX IF NOT EXISTS bvk_ticket_topic_history ON bvk_tickets(topic_chat_id,topic_id,id DESC) WHERE topic_id>0;
CREATE UNIQUE INDEX IF NOT EXISTS bvk_active_topic ON bvk_tickets(topic_chat_id,topic_id)
 WHERE topic_id>0 AND status IN ('in_progress','waiting_client');
