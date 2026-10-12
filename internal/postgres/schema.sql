CREATE TABLE IF NOT EXISTS bvk_companies (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS bvk_users (id bigint PRIMARY KEY, payload jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS bvk_tickets (
 id bigserial PRIMARY KEY, client_id bigint NOT NULL REFERENCES bvk_users(id), company_id bigint NOT NULL REFERENCES bvk_companies(id),
 status text NOT NULL CHECK (status IN ('new','in_progress','waiting_client','closed','cancelled')),
 department text NOT NULL CHECK (department IN ('it','1c','installation','field')),
 version bigint NOT NULL, topic_chat_id bigint NOT NULL DEFAULT 0, topic_id bigint NOT NULL DEFAULT 0, payload jsonb NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS bvk_active_client ON bvk_tickets(client_id) WHERE status IN ('new','in_progress','waiting_client');
CREATE UNIQUE INDEX IF NOT EXISTS bvk_topic ON bvk_tickets(topic_chat_id,topic_id) WHERE topic_id <> 0;
CREATE TABLE IF NOT EXISTS bvk_messages (id bigserial PRIMARY KEY, ticket_id bigint NOT NULL REFERENCES bvk_tickets(id), telegram_chat_id bigint NOT NULL DEFAULT 0, telegram_message_id bigint NOT NULL DEFAULT 0, direction text NOT NULL DEFAULT '', payload jsonb NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS bvk_message_source ON bvk_messages(telegram_chat_id,telegram_message_id,direction) WHERE telegram_message_id<>0;
CREATE INDEX IF NOT EXISTS bvk_messages_ticket ON bvk_messages(ticket_id,id);
CREATE TABLE IF NOT EXISTS bvk_events (id bigserial PRIMARY KEY, ticket_id bigint NOT NULL REFERENCES bvk_tickets(id), payload jsonb NOT NULL);
CREATE INDEX IF NOT EXISTS bvk_events_ticket ON bvk_events(ticket_id,id);
CREATE TABLE IF NOT EXISTS bvk_access (id bigserial PRIMARY KEY, user_id bigint NOT NULL REFERENCES bvk_users(id), company_id bigint NOT NULL REFERENCES bvk_companies(id), status text NOT NULL DEFAULT 'pending', created_at timestamptz NOT NULL DEFAULT now());
CREATE UNIQUE INDEX IF NOT EXISTS bvk_access_pending ON bvk_access(user_id) WHERE status='pending';
CREATE TABLE IF NOT EXISTS bvk_updates (id bigint PRIMARY KEY, payload jsonb NOT NULL, finished boolean NOT NULL DEFAULT false);
CREATE TABLE IF NOT EXISTS bvk_outbox (id bigserial PRIMARY KEY, kind text NOT NULL, ticket_id bigint NOT NULL DEFAULT 0, payload jsonb NOT NULL, attempts integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(), last_error text NOT NULL DEFAULT '', state text NOT NULL DEFAULT 'pending', lease_until timestamptz);
CREATE INDEX IF NOT EXISTS bvk_outbox_ready ON bvk_outbox(available_at,id) WHERE state IN ('pending','processing');
CREATE INDEX IF NOT EXISTS bvk_outbox_ticket_order ON bvk_outbox(ticket_id,id) WHERE state IN ('pending','processing');
