-- Keep UI and ticket notifications in one durable conversation order, including
-- across completion/reopening. No historic records or attachments are removed.
ALTER TABLE bvk_outbox ADD COLUMN IF NOT EXISTS conversation_id bigint NOT NULL DEFAULT 0;
UPDATE bvk_outbox o SET conversation_id=COALESCE(
 (SELECT client_id FROM bvk_tickets WHERE id=o.ticket_id),
 NULLIF((payload->>'user_id')::bigint,0),
 CASE WHEN (payload->>'chat_id')::bigint>0 THEN (payload->>'chat_id')::bigint END,0)
WHERE state IN ('pending','processing','failed');
CREATE INDEX IF NOT EXISTS bvk_outbox_conversation_order ON bvk_outbox(conversation_id,id)
 WHERE state IN ('pending','processing');
CREATE INDEX IF NOT EXISTS bvk_updates_unfinished ON bvk_updates(id) WHERE NOT finished;
CREATE INDEX IF NOT EXISTS bvk_tickets_client ON bvk_tickets(client_id);

-- Compare typed IDs to indexed columns, not text casts of every stored row.
-- Damaged/unsupported payloads must not break selection of the entire inbox.
CREATE OR REPLACE FUNCTION bvk_update_number(value text) RETURNS bigint
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
 RETURN COALESCE(value::bigint,0);
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
 RETURN 0;
END $$;
CREATE OR REPLACE FUNCTION bvk_update_keys(p jsonb) RETURNS text[] LANGUAGE plpgsql STABLE AS $$
DECLARE m jsonb; actor bigint; chat bigint; topic bigint; tid text; command text[]; keys text[] := ARRAY[]::text[];
BEGIN
 m := COALESCE(p->'message',p->'edited_message',p->'callback_query'->'message','{}'::jsonb);
 actor := bvk_update_number(COALESCE(p->'callback_query'->'from'->>'id',m->'from'->>'id'));
 chat := bvk_update_number(m->'chat'->>'id'); topic := bvk_update_number(m->>'message_thread_id');
 keys := array_append(keys,'actor:'||actor);
 IF chat <> 0 AND chat <> actor AND p->'callback_query' IS NULL THEN keys := array_append(keys,'topic:'||chat||':'||topic); END IF;
 IF COALESCE(p->'callback_query'->>'data','') ~ '^(claim|close|transfer|transferdept|cancel|cancelconfirm|promptcancel|reopen|waiting|resume):[0-9]+' THEN
  tid := split_part(p->'callback_query'->>'data',':',2); keys := array_append(keys,'ticket:'||tid);
 END IF;
 command := regexp_match(COALESCE(m->>'text',''), '^/(close|transfer|assign|cancel|bind|reopen) +([0-9]+)( |$)');
 IF command IS NOT NULL THEN keys := array_append(keys,'ticket:'||command[2]); END IF;
 FOR tid IN SELECT payload->>'TicketID' FROM bvk_workflows WHERE user_id=actor AND chat_id=chat AND topic_id=topic AND scenario='active_action' LOOP
  IF tid IS NOT NULL THEN keys := array_append(keys,'ticket:'||tid); END IF;
 END LOOP;
 IF p->'edited_message' IS NOT NULL THEN
  FOR tid IN SELECT ticket_id::text FROM bvk_messages WHERE telegram_chat_id=chat AND telegram_message_id=bvk_update_number(m->>'message_id') LOOP
   keys := array_append(keys,'ticket:'||tid);
  END LOOP;
 END IF;
 FOR tid IN SELECT id::text FROM bvk_tickets WHERE client_id=actor OR (topic_chat_id=chat AND topic_id=topic AND topic_id<>0) LOOP
  keys := array_append(keys,'ticket:'||tid);
 END LOOP;
 RETURN keys;
END $$;
