-- Only presentation refresh: preserve historical messages, topics and archives.
INSERT INTO bvk_outbox(kind,ticket_id,payload,available_at)
SELECT 'refresh_card',id,'{}'::jsonb,now() FROM bvk_tickets
WHERE COALESCE((payload->>'queue_message_id')::bigint,0)>0
 OR COALESCE((payload->>'topic_message_id')::bigint,0)>0;
