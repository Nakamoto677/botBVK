WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), windows AS (
 SELECT 'current' AS kind,lo,hi FROM p
 UNION ALL SELECT 'previous',lo-GREATEST(hi-lo,interval '0 seconds'),lo FROM p
), counts AS (
 SELECT w.kind,q.* FROM windows w CROSS JOIN LATERAL (SELECT 'Поступило' AS metric, count(*) AS n FROM (WITH p AS (SELECT w.lo,w.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT t.id AS ticket_id, e.department, t.first_assignee_id AS actor_id, t.created_at AS at FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true, p WHERE t.created_at>=p.lo AND t.created_at<p.hi) SELECT DISTINCT q.ticket_id FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true [[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) selected UNION ALL SELECT 'Закрыто' AS metric, count(*) AS n FROM (WITH p AS (SELECT w.lo,w.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT ticket_id, department, actor_id, occurred_at AS at,cycle_number FROM last_closed) SELECT DISTINCT q.ticket_id FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true [[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) selected UNION ALL SELECT 'Отменено' AS metric, count(*) AS n FROM (WITH p AS (SELECT w.lo,w.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT DISTINCT ON (ticket_id) ticket_id,department,actor_id,occurred_at AS at FROM analytics.events,p WHERE kind='cancelled' AND occurred_at>=p.lo AND occurred_at<p.hi ORDER BY ticket_id,occurred_at DESC,id DESC) SELECT DISTINCT q.ticket_id FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true [[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) selected) q
)
SELECT c.metric AS "Показатель",c.n AS "За период",b.n AS "Ранее",c.n-b.n AS "Изменение"
FROM counts c JOIN counts b ON b.metric=c.metric AND b.kind='previous'
WHERE c.kind='current' ORDER BY CASE c.metric WHEN 'Поступило' THEN 1 WHEN 'Закрыто' THEN 2 ELSE 3 END
