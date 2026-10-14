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
), q AS (SELECT ticket_id,department,assignee_id AS actor_id,p.hi AS at FROM analytics.state_intervals,p WHERE started_at<p.hi AND (ended_at IS NULL OR ended_at>=p.hi) AND status IN ('new','in_progress','waiting_client'))
SELECT coalesce(d.name,'Отдел неизвестен') AS "Отдел",count(*) AS "Заявок" FROM q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.departments d ON d.code=q.department LEFT JOIN analytics.companies c ON c.id=t.company_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]] GROUP BY coalesce(d.name,'Отдел неизвестен') ORDER BY count(*) DESC
