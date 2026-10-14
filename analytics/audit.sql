-- Read-only preflight on an isolated restored copy of the bot database.
-- Returns aggregate metadata, never message bodies, tokens or credentials.
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT count(*) AS tickets,
 count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM public.bvk_events e WHERE e.ticket_id=t.id AND e.payload->>'Kind'='created')) AS missing_creation_events,
 count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM public.bvk_events e WHERE e.ticket_id=t.id)) AS without_events,
 count(*) FILTER (WHERE t.payload->>'created_at' IS NULL) AS missing_creation_date
FROM public.bvk_tickets t;
SELECT payload->>'Kind' AS event_kind,count(*) AS events,
 count(*) FILTER(WHERE payload->>'CreatedAt' IS NULL) AS missing_date,
 count(*) FILTER(WHERE payload->>'Kind' IN ('closed','cancelled','claimed','reassigned','transferred') AND coalesce(payload->>'ActorID','0')='0') AS missing_actor
FROM public.bvk_events GROUP BY payload->>'Kind' ORDER BY event_kind;
SELECT count(*) AS deliveries,
 count(*) FILTER(WHERE NOT EXISTS(SELECT 1 FROM public.bvk_messages m WHERE m.ticket_id=e.ticket_id AND m.id::text=e.payload->'Data'->>'source_message_id') AND coalesce(e.payload->'Data'->>'source_message_id','0')<>'0') AS missing_source_messages
FROM public.bvk_events e WHERE e.payload->>'Kind'='message_delivered';
SELECT count(*) AS transfers,
 count(*) FILTER(WHERE payload->'Data'->>'from' IS NULL OR payload->'Data'->>'to' IS NULL) AS missing_departments
FROM public.bvk_events WHERE payload->>'Kind'='transferred';
COMMIT;
