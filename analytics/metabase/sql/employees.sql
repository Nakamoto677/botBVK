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
), closures AS (
 SELECT q.actor_id,count(*) AS n, avg(c.work_seconds)/3600 AS avg_h,
 percentile_cont(0.5) WITHIN GROUP(ORDER BY c.work_seconds)/3600 AS median_h,min(c.work_seconds)/3600 AS min_h,max(c.work_seconds)/3600 AS max_h,count(c.work_seconds) AS known
 FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.cycles c ON c.ticket_id=q.ticket_id AND c.number=q.cycle_number
 WHERE true [[ AND q.department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY q.actor_id
), load AS (
 SELECT t.assignee_id,count(*) AS n,count(*) FILTER(WHERE t.status='waiting_client') AS waiting
 FROM analytics.tickets t WHERE t.status IN ('new','in_progress','waiting_client') [[ AND t.department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY t.assignee_id
), replies AS (
 SELECT c.first_response_by,avg(c.first_response_work_seconds)/60 AS avg_m,
 percentile_cont(0.5) WITHIN GROUP(ORDER BY c.first_response_work_seconds)/60 AS median_m,count(c.first_response_work_seconds) AS known
 FROM analytics.cycles c JOIN analytics.tickets t ON t.id=c.ticket_id,p
 WHERE c.first_response_at>=p.lo AND c.first_response_at<p.hi [[ AND c.first_response_department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY c.first_response_by
), transfers AS (
 SELECT q.actor_id,count(*) AS n FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id,p
 WHERE q.kind='transferred' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND q.from_department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY q.actor_id
), returns AS (
 SELECT prev.actor_id,count(*) AS n FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id
 JOIN LATERAL (SELECT actor_id,department FROM analytics.events e WHERE e.ticket_id=q.ticket_id AND e.kind='closed' AND (e.occurred_at,e.id)<(q.occurred_at,q.id) ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1) prev ON true,p
 WHERE q.kind='reopened' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND prev.department={{department}} ]] [[ AND t.company_id={{company}} ]] GROUP BY prev.actor_id
), people AS (
 SELECT id,name,departments FROM analytics.employees WHERE role IN ('engineer','admin','dispatcher')
 UNION SELECT NULL::bigint,'Не назначен / автор неизвестен',ARRAY[]::text[]
 UNION SELECT actor_id,'Неизвестный сотрудник #'||actor_id,ARRAY[]::text[] FROM (SELECT actor_id FROM closures UNION SELECT assignee_id FROM load UNION SELECT first_response_by FROM replies UNION SELECT actor_id FROM transfers UNION SELECT actor_id FROM returns) x WHERE actor_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM analytics.employees e WHERE e.id=x.actor_id)
)
SELECT u.id AS "ID сотрудника",u.name AS "Сотрудник",array_to_string(u.departments,', ') AS "Текущие отделы",
 coalesce(c.n,0) AS "Закрыл за период",coalesce(l.n,0) AS "Назначено сейчас",coalesce(l.waiting,0) AS "Ожидают клиента",
 c.avg_h AS "Решение: среднее, раб. ч",c.median_h AS "Решение: медиана, раб. ч",c.min_h AS "Решение: мин, раб. ч",c.max_h AS "Решение: макс, раб. ч",coalesce(c.known,0) AS "Решение: выборка",
 r.avg_m AS "Первый ответ: среднее, раб. мин",r.median_m AS "Первый ответ: медиана, раб. мин",coalesce(r.known,0) AS "Ответ: выборка",coalesce(ret.n,0) AS "Возвраты после его закрытий",coalesce(tr.n,0) AS "Передачи"
FROM people u LEFT JOIN closures c ON c.actor_id IS NOT DISTINCT FROM u.id LEFT JOIN load l ON l.assignee_id IS NOT DISTINCT FROM u.id
LEFT JOIN replies r ON r.first_response_by IS NOT DISTINCT FROM u.id LEFT JOIN transfers tr ON tr.actor_id IS NOT DISTINCT FROM u.id LEFT JOIN returns ret ON ret.actor_id IS NOT DISTINCT FROM u.id
WHERE true [[ AND u.id={{employee}} ]] ORDER BY coalesce(c.n,0) DESC,u.name
