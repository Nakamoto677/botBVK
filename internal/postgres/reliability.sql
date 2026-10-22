ALTER TABLE bvk_updates ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 0;
ALTER TABLE bvk_updates ADD COLUMN IF NOT EXISTS available_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE bvk_updates ADD COLUMN IF NOT EXISTS quarantined boolean NOT NULL DEFAULT false;
ALTER TABLE bvk_updates ADD COLUMN IF NOT EXISTS last_error text NOT NULL DEFAULT '';
ALTER TABLE bvk_drafts DROP CONSTRAINT IF EXISTS bvk_drafts_department_check;
ALTER TABLE bvk_drafts ADD CONSTRAINT bvk_drafts_department_check CHECK(department IN ('','it','1c','installation','field'));
ALTER TABLE bvk_drafts ADD COLUMN IF NOT EXISTS state jsonb;
CREATE UNIQUE INDEX IF NOT EXISTS bvk_ui_idempotency ON bvk_outbox ((payload->>'ui_key')) WHERE kind='ui_screen';
-- Resolve dependencies on every selection: a ticket created after receipt still
-- joins its client's lane. Quarantine deliberately remains unfinished for review.
CREATE OR REPLACE FUNCTION bvk_update_keys(p jsonb) RETURNS text[] LANGUAGE plpgsql STABLE AS $$
DECLARE m jsonb; actor text; chat text; topic text; tid text; command text[]; keys text[] := ARRAY[]::text[];
BEGIN
 m := COALESCE(p->'message',p->'edited_message',p->'callback_query'->'message','{}'::jsonb);
 actor := COALESCE(p->'callback_query'->'from'->>'id',m->'from'->>'id','0');
 chat := COALESCE(m->'chat'->>'id','0'); topic := COALESCE(m->>'message_thread_id','0');
 keys := array_append(keys,'actor:'||actor);
 IF chat <> '0' AND chat <> actor AND p->'callback_query' IS NULL THEN keys := array_append(keys,'topic:'||chat||':'||topic); END IF;
 IF COALESCE(p->'callback_query'->>'data','') ~ '^(claim|close|transfer|transferdept|cancel|cancelconfirm|promptcancel|reopen|waiting|resume):[0-9]+' THEN
  tid := split_part(p->'callback_query'->>'data',':',2); keys := array_append(keys,'ticket:'||tid);
 END IF;
 command := regexp_match(COALESCE(m->>'text',''), '^/(close|transfer|assign|cancel|bind|reopen) +([0-9]+)( |$)');
 IF command IS NOT NULL THEN keys := array_append(keys,'ticket:'||command[2]); END IF;
 FOR tid IN SELECT payload->>'TicketID' FROM bvk_workflows WHERE user_id::text=actor AND chat_id::text=chat AND topic_id::text=topic AND scenario='active_action' LOOP
  IF tid IS NOT NULL THEN keys := array_append(keys,'ticket:'||tid); END IF;
 END LOOP;
 IF p->'edited_message' IS NOT NULL THEN
  FOR tid IN SELECT ticket_id::text FROM bvk_messages WHERE telegram_chat_id::text=chat AND telegram_message_id::text=m->>'message_id' LOOP
   keys := array_append(keys,'ticket:'||tid);
  END LOOP;
 END IF;
 FOR tid IN SELECT id::text FROM bvk_tickets WHERE client_id::text=actor OR (topic_chat_id::text=chat AND topic_id::text=topic AND topic_id<>0) LOOP
  keys := array_append(keys,'ticket:'||tid);
 END LOOP;
 RETURN keys;
END $$;
CREATE TABLE IF NOT EXISTS bvk_send_parts (key text NOT NULL,part integer NOT NULL,message_id bigint NOT NULL,PRIMARY KEY(key,part));

CREATE UNIQUE INDEX IF NOT EXISTS bvk_notice_idempotency ON bvk_outbox ((payload->>'notice_key')) WHERE kind='access_notification';
