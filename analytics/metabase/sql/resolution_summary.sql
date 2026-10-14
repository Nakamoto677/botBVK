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
), selected AS (SELECT c.* FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.cycles c ON c.ticket_id=q.ticket_id AND c.number=q.cycle_number WHERE true [[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) SELECT 'Решение · рабочие часы' AS "Показатель",percentile_cont(0.5) WITHIN GROUP(ORDER BY work_seconds)/3600 AS "Медиана",avg(work_seconds)/3600 AS "Среднее",min(work_seconds)/3600 AS "Минимум",max(work_seconds)/3600 AS "Максимум",count(work_seconds) AS "Выборка" FROM selected UNION ALL SELECT 'Решение · календарные часы' AS "Показатель",percentile_cont(0.5) WITHIN GROUP(ORDER BY calendar_seconds)/3600 AS "Медиана",avg(calendar_seconds)/3600 AS "Среднее",min(calendar_seconds)/3600 AS "Минимум",max(calendar_seconds)/3600 AS "Максимум",count(calendar_seconds) AS "Выборка" FROM selected UNION ALL SELECT 'Ожидание клиента · рабочие часы' AS "Показатель",percentile_cont(0.5) WITHIN GROUP(ORDER BY waiting_work_seconds)/3600 AS "Медиана",avg(waiting_work_seconds)/3600 AS "Среднее",min(waiting_work_seconds)/3600 AS "Минимум",max(waiting_work_seconds)/3600 AS "Максимум",count(waiting_work_seconds) AS "Выборка" FROM selected UNION ALL SELECT 'Без ожидания · рабочие часы' AS "Показатель",percentile_cont(0.5) WITHIN GROUP(ORDER BY work_without_waiting_seconds)/3600 AS "Медиана",avg(work_without_waiting_seconds)/3600 AS "Среднее",min(work_without_waiting_seconds)/3600 AS "Минимум",max(work_without_waiting_seconds)/3600 AS "Максимум",count(work_without_waiting_seconds) AS "Выборка" FROM selected
