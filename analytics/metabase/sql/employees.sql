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
), measured AS (
 SELECT q.actor_id,CASE WHEN assignment.kind IN ('claimed','reassigned')
  AND assignment.assignee_id=q.actor_id AND NOT assignment.incomplete AND NOT t.incomplete
  THEN EXTRACT(EPOCH FROM (q.occurred_at-assignment.occurred_at)) END AS seconds
 FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id
 LEFT JOIN LATERAL (
  SELECT e.kind,e.assignee_id,e.occurred_at,e.incomplete FROM analytics.events e
  WHERE e.ticket_id=q.ticket_id AND (e.occurred_at,e.id)<(q.occurred_at,q.id)
   AND e.kind IN ('claimed','reassigned','transferred','reopened','closed','cancelled')
  ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1
 ) assignment ON true
 WHERE true [[ AND q.department={{department}} ]] [[ AND t.company_id={{company}} ]]
), closures AS (
 SELECT actor_id,count(*) AS n,avg(seconds) AS avg_seconds,percentile_cont(0.5) WITHIN GROUP(ORDER BY seconds) AS median_seconds,max(seconds) AS max_seconds,count(seconds) AS known
 FROM measured GROUP BY actor_id
), load AS (
 SELECT t.assignee_id,count(*) AS n FROM analytics.tickets t
 WHERE t.status IN ('new','in_progress','waiting_client') AND t.assignee_id IS NOT NULL
 [[ AND t.department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY t.assignee_id
), people AS (
 SELECT id,name FROM analytics.employees WHERE role IN ('engineer','admin','dispatcher')
 UNION SELECT NULL::bigint,'Автор неизвестен' WHERE EXISTS(SELECT 1 FROM closures WHERE actor_id IS NULL)
 UNION SELECT actor_id,'Неизвестный сотрудник #'||actor_id FROM
  (SELECT actor_id FROM closures UNION SELECT assignee_id FROM load) x
  WHERE actor_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM analytics.employees e WHERE e.id=x.actor_id)
)
SELECT u.id AS "ID сотрудника",u.name AS "Сотрудник",coalesce(c.n,0) AS "Закрыто за период",
 coalesce(l.n,0) AS "В работе сейчас",
coalesce(CASE WHEN c.avg_seconds IS NULL THEN NULL WHEN c.avg_seconds<60 THEN '< 1 мин' WHEN floor((c.avg_seconds) / 60)::bigint<60 THEN floor((c.avg_seconds) / 60)::bigint||' мин' ELSE (floor((c.avg_seconds) / 60)::bigint / 60)||' '||CASE WHEN (floor((c.avg_seconds) / 60)::bigint / 60)%10=1 AND (floor((c.avg_seconds) / 60)::bigint / 60)%100<>11 THEN 'час' WHEN (floor((c.avg_seconds) / 60)::bigint / 60)%10 BETWEEN 2 AND 4 AND (floor((c.avg_seconds) / 60)::bigint / 60)%100 NOT BETWEEN 12 AND 14 THEN 'часа' ELSE 'часов' END ||CASE WHEN (floor((c.avg_seconds) / 60)::bigint % 60)=0 THEN '' ELSE ' '||(floor((c.avg_seconds) / 60)::bigint % 60)||' мин' END END,'—') AS "Среднее время выполнения",
coalesce(CASE WHEN c.median_seconds IS NULL THEN NULL WHEN c.median_seconds<60 THEN '< 1 мин' WHEN floor((c.median_seconds) / 60)::bigint<60 THEN floor((c.median_seconds) / 60)::bigint||' мин' ELSE (floor((c.median_seconds) / 60)::bigint / 60)||' '||CASE WHEN (floor((c.median_seconds) / 60)::bigint / 60)%10=1 AND (floor((c.median_seconds) / 60)::bigint / 60)%100<>11 THEN 'час' WHEN (floor((c.median_seconds) / 60)::bigint / 60)%10 BETWEEN 2 AND 4 AND (floor((c.median_seconds) / 60)::bigint / 60)%100 NOT BETWEEN 12 AND 14 THEN 'часа' ELSE 'часов' END ||CASE WHEN (floor((c.median_seconds) / 60)::bigint % 60)=0 THEN '' ELSE ' '||(floor((c.median_seconds) / 60)::bigint % 60)||' мин' END END,'—') AS "Медианное время выполнения",
coalesce(CASE WHEN c.max_seconds IS NULL THEN NULL WHEN c.max_seconds<60 THEN '< 1 мин' WHEN floor((c.max_seconds) / 60)::bigint<60 THEN floor((c.max_seconds) / 60)::bigint||' мин' ELSE (floor((c.max_seconds) / 60)::bigint / 60)||' '||CASE WHEN (floor((c.max_seconds) / 60)::bigint / 60)%10=1 AND (floor((c.max_seconds) / 60)::bigint / 60)%100<>11 THEN 'час' WHEN (floor((c.max_seconds) / 60)::bigint / 60)%10 BETWEEN 2 AND 4 AND (floor((c.max_seconds) / 60)::bigint / 60)%100 NOT BETWEEN 12 AND 14 THEN 'часа' ELSE 'часов' END ||CASE WHEN (floor((c.max_seconds) / 60)::bigint % 60)=0 THEN '' ELSE ' '||(floor((c.max_seconds) / 60)::bigint % 60)||' мин' END END,'—') AS "Максимальное время выполнения",
 coalesce(c.known,0) AS "Измеренных сроков",c.avg_seconds AS "Среднее, секунды",c.median_seconds AS "Медиана, секунды",c.max_seconds AS "Максимум, секунды"
FROM people u LEFT JOIN closures c ON c.actor_id IS NOT DISTINCT FROM u.id
LEFT JOIN load l ON l.assignee_id=u.id
WHERE true [[ AND u.id={{employee}} ]] ORDER BY coalesce(c.n,0) DESC,u.name
