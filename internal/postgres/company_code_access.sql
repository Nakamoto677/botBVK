ALTER TABLE bvk_access ADD COLUMN method text NOT NULL DEFAULT 'admin';
ALTER TABLE bvk_access ADD COLUMN previous_company_id bigint REFERENCES bvk_companies(id);
UPDATE bvk_access SET status='superseded' WHERE status='pending';
UPDATE bvk_outbox SET state='done', lease_until=NULL WHERE kind='access_request_notification' AND state IN ('pending','processing','failed');

CREATE TABLE bvk_company_code_attempts (
 user_id bigint PRIMARY KEY,
 attempts integer NOT NULL,
 window_started_at timestamptz NOT NULL,
 blocked_until timestamptz
);
