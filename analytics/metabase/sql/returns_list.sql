WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
) SELECT t.id AS "Номер",q.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Повторно открыта",u.name AS "Предыдущий закрывший",prev.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Предыдущее закрытие",d.name AS "Отдел закрытия",co.name AS "Компания"
FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id
JOIN LATERAL (SELECT actor_id,department,occurred_at FROM analytics.events e WHERE e.ticket_id=q.ticket_id AND e.kind='closed' AND (e.occurred_at,e.id)<(q.occurred_at,q.id) ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1) prev ON true
LEFT JOIN analytics.employees u ON u.id=prev.actor_id LEFT JOIN analytics.departments d ON d.code=prev.department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE q.kind='reopened' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND prev.department={{department}} ]] [[ AND prev.actor_id={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY q.occurred_at,q.id
