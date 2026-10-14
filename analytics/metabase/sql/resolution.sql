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
) SELECT count(c.calendar_seconds) AS "Решение, кал. ч: выборка",
avg(c.calendar_seconds)/3600 AS "Решение, кал. ч: среднее",
percentile_cont(0.5) WITHIN GROUP (ORDER BY c.calendar_seconds)/3600 AS "Решение, кал. ч: медиана",
min(c.calendar_seconds)/3600 AS "Решение, кал. ч: мин",
max(c.calendar_seconds)/3600 AS "Решение, кал. ч: макс",
count(c.work_seconds) AS "Решение, раб. ч: выборка",
avg(c.work_seconds)/3600 AS "Решение, раб. ч: среднее",
percentile_cont(0.5) WITHIN GROUP (ORDER BY c.work_seconds)/3600 AS "Решение, раб. ч: медиана",
min(c.work_seconds)/3600 AS "Решение, раб. ч: мин",
max(c.work_seconds)/3600 AS "Решение, раб. ч: макс",
count(c.work_without_waiting_seconds) AS "Без ожидания, раб. ч: выборка",
avg(c.work_without_waiting_seconds)/3600 AS "Без ожидания, раб. ч: среднее",
percentile_cont(0.5) WITHIN GROUP (ORDER BY c.work_without_waiting_seconds)/3600 AS "Без ожидания, раб. ч: медиана",
min(c.work_without_waiting_seconds)/3600 AS "Без ожидания, раб. ч: мин",
max(c.work_without_waiting_seconds)/3600 AS "Без ожидания, раб. ч: макс",
count(c.waiting_work_seconds) AS "Ожидание, раб. ч: выборка",
avg(c.waiting_work_seconds)/3600 AS "Ожидание, раб. ч: среднее",
percentile_cont(0.5) WITHIN GROUP (ORDER BY c.waiting_work_seconds)/3600 AS "Ожидание, раб. ч: медиана",
min(c.waiting_work_seconds)/3600 AS "Ожидание, раб. ч: мин",
max(c.waiting_work_seconds)/3600 AS "Ожидание, раб. ч: макс" FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.cycles c ON c.ticket_id=q.ticket_id AND c.number=q.cycle_number WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]
