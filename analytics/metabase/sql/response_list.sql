WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
) SELECT t.id AS "Номер",c.number AS "Цикл",c.first_response_at AT TIME ZONE 'Asia/Irkutsk' AS "Ответ доставлен",coalesce(u.name,'Участник #'||c.first_response_by) AS "Ответил",d.name AS "Отдел ответа",co.name AS "Компания",c.first_response_seconds/60 AS "Первый ответ, кал. мин",c.first_response_work_seconds/60 AS "Первый ответ, раб. мин",c.incomplete AS "Неполная история"
FROM analytics.cycles c JOIN analytics.tickets t ON t.id=c.ticket_id LEFT JOIN analytics.employees u ON u.id=c.first_response_by LEFT JOIN analytics.departments d ON d.code=c.first_response_department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE c.first_response_at>=p.lo AND c.first_response_at<p.hi [[ AND c.first_response_department={{department}} ]] [[ AND c.first_response_by={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY c.first_response_at,t.id,c.number
