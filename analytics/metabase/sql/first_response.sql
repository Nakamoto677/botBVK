WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
) SELECT count(c.first_response_work_seconds) AS "Выборка",
 avg(c.first_response_work_seconds)/60 AS "Среднее, раб. мин",
 percentile_cont(0.5) WITHIN GROUP(ORDER BY c.first_response_work_seconds)/60 AS "Медиана, раб. мин",
 avg(c.first_response_seconds)/60 AS "Среднее, кал. мин"
FROM analytics.cycles c JOIN analytics.tickets t ON t.id=c.ticket_id,p
WHERE c.first_response_at>=p.lo AND c.first_response_at<p.hi
[[ AND c.first_response_department={{department}} ]] [[ AND c.first_response_by={{employee}} ]] [[ AND t.company_id={{company}} ]]
