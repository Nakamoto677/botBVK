WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
) SELECT CASE q.kind WHEN 'closed' THEN 'Закрытия' WHEN 'cancelled' THEN 'Отмены' WHEN 'reopened' THEN 'Повторные открытия' WHEN 'transferred' THEN 'Передачи' END AS "Событие",count(*) AS "Операций" FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id,p WHERE q.kind IN ('closed','cancelled','reopened','transferred') AND q.occurred_at>=p.lo AND q.occurred_at<p.hi
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]] GROUP BY q.kind ORDER BY q.kind
