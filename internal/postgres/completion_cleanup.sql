-- Historical thirty-day delete operations must not become active at rollout.
-- New deletes require an explicit completion intent with an exact topic binding.
-- Keep the outbox rows for auditing instead of removing history.
UPDATE bvk_outbox SET state='done',lease_until=NULL,
 last_error='Superseded by verified completion cleanup; historical topic retained'
WHERE kind='delete_topic' AND state IN ('pending','processing','failed')
 AND NOT EXISTS (
  SELECT 1 FROM bvk_events e WHERE e.ticket_id=bvk_outbox.ticket_id
   AND e.payload->>'Kind'='completion_cleanup_requested'
   AND e.payload->'Data'->>'completion_key'=bvk_outbox.payload->>'completion_key'
   AND e.payload->'Data'->>'chat_id'=bvk_outbox.payload->>'chat_id'
   AND e.payload->'Data'->>'topic_id'=bvk_outbox.payload->>'topic_id'
 );

-- Refresh existing cards once to remove obsolete reply controls on upgrade.
INSERT INTO bvk_outbox(kind,ticket_id,payload,available_at)
SELECT 'refresh_card',id,'{}'::jsonb,now() FROM bvk_tickets
WHERE COALESCE((payload->>'queue_message_id')::bigint,0)>0
 OR COALESCE((payload->>'topic_message_id')::bigint,0)>0;
