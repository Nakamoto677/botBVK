WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), days AS (SELECT generate_series(start_day::timestamp,LEAST(end_day,(SELECT (snapshot_at AT TIME ZONE 'Asia/Irkutsk')::date FROM analytics.refresh_state WHERE id=1))::timestamp,interval '1 day')::date AS day FROM bounds)
SELECT day AS "Дата",
 (SELECT count(*) FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true
 WHERE t.created_at>=day::timestamp AT TIME ZONE 'Asia/Irkutsk' AND t.created_at<(day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk'
 [[ AND e.department={{department}} ]] [[ AND t.first_assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]) AS "Поступило",
 (SELECT count(*) FROM (SELECT DISTINCT ON(ticket_id) * FROM analytics.events WHERE kind='closed' AND occurred_at>=day::timestamp AT TIME ZONE 'Asia/Irkutsk' AND occurred_at<(day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk' ORDER BY ticket_id,occurred_at DESC,id DESC) q
 JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]
 ) AS "Закрыто уникальных за день",
 (SELECT count(*) FROM analytics.state_intervals q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.started_at<LEAST((day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk',(SELECT snapshot_at FROM analytics.refresh_state WHERE id=1)) AND (q.ended_at IS NULL OR q.ended_at>=LEAST((day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk',(SELECT snapshot_at FROM analytics.refresh_state WHERE id=1))) AND q.status IN ('new','in_progress','waiting_client')
[[ AND q.department = {{department}} ]]
[[ AND q.assignee_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]
 ) AS "Очередь на конец дня" FROM days ORDER BY day
