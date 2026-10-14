WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
) SELECT t.id AS "Номер",q.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Передана",u.name AS "Передал",f.name AS "Из отдела",d.name AS "В отдел",co.name AS "Компания",q.detail AS "Причина",q.incomplete AS "Неполная история"
FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.employees u ON u.id=q.actor_id LEFT JOIN analytics.departments f ON f.code=q.from_department LEFT JOIN analytics.departments d ON d.code=q.to_department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE q.kind='transferred' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND q.from_department={{department}} ]] [[ AND q.actor_id={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY q.occurred_at,q.id
