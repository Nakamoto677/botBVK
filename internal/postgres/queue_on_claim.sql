-- Retire legacy dispatcher and duty notifications without deleting audit data.
UPDATE bvk_outbox SET state='done',lease_until=NULL
WHERE state IN ('pending','processing') AND
 (kind='dispatcher_message' OR
  (kind='access_notification' AND payload->>'text' LIKE 'Аварийная заявка #%'));

-- Messages already delivered to a legacy topic remain there. Only undelivered
-- client messages need replay after the engineer claims the queue card.
UPDATE bvk_messages m SET payload=jsonb_set(m.payload,'{deferred_delivery}','true'::jsonb)
FROM bvk_tickets t
WHERE m.ticket_id=t.id AND t.status='new'
 AND COALESCE((t.payload->>'assignee_id')::bigint,0)=0
 AND m.direction='inbound'
 AND NOT EXISTS (
  SELECT 1 FROM bvk_events e WHERE e.ticket_id=t.id
   AND e.payload->>'Kind'='message_delivered'
   AND e.payload->'Data'->>'source_message_id'=m.id::text
 );

-- Pending delivery to a not-yet-claimed topic must not block its future claim.
UPDATE bvk_outbox o SET state='done',lease_until=NULL
FROM bvk_tickets t
WHERE o.ticket_id=t.id AND t.status='new'
 AND COALESCE((t.payload->>'assignee_id')::bigint,0)=0
 AND o.state IN ('pending','processing')
 AND o.kind IN ('create_topic','team_message');

-- Keep historical bindings and pause their topics. A version marker makes this
-- enqueue one-time, including when a future restart sees a still-unclaimed card.
INSERT INTO bvk_outbox(kind,ticket_id,payload,available_at)
SELECT 'close_topic',id,jsonb_build_object('chat_id',topic_chat_id,'topic_id',topic_id,'pause',true),now()
FROM bvk_tickets WHERE status='new' AND COALESCE((payload->>'assignee_id')::bigint,0)=0 AND topic_id<>0;

-- Replace stale card labels and emergency buttons for active and terminal
-- tickets. The enclosing migration marker protects this enqueue on restart.
INSERT INTO bvk_outbox(kind,ticket_id,payload,available_at)
SELECT 'refresh_card',id,'{}'::jsonb,now()
FROM bvk_tickets
WHERE COALESCE((payload->>'queue_message_id')::bigint,0)>0
 OR COALESCE((payload->>'topic_message_id')::bigint,0)>0;

-- Update both optimistic-lock representations only when the current payload
-- changes. Emergency events and messages intentionally remain historical data.
UPDATE bvk_tickets
SET version=version+1,
 payload=jsonb_set(
  CASE WHEN status='new' AND COALESCE((payload->>'assignee_id')::bigint,0)=0 AND payload->>'routing_pending'='true'
   THEN jsonb_set(payload-'emergency','{routing_pending}','false'::jsonb)
   ELSE payload-'emergency' END,
  '{version}',to_jsonb(version+1))
WHERE payload ? 'emergency' OR
 (status='new' AND COALESCE((payload->>'assignee_id')::bigint,0)=0 AND payload->>'routing_pending'='true');
