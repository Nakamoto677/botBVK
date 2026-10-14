WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT t.id AS ticket_id, e.department, t.first_assignee_id AS actor_id, t.created_at AS at FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true, p WHERE t.created_at>=p.lo AND t.created_at<p.hi)
SELECT coalesce(c.name,'Компания неизвестна') AS "Компания",count(*) AS "Заявок" FROM q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.departments d ON d.code=q.department LEFT JOIN analytics.companies c ON c.id=t.company_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]] GROUP BY coalesce(c.name,'Компания неизвестна') ORDER BY count(*) DESC
