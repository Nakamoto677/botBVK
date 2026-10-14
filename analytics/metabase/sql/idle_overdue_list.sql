WITH selected AS (WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT t.id AS ticket_id,t.department,t.assignee_id AS actor_id,t.created_at AS at FROM analytics.tickets t WHERE t.status='in_progress' AND t.idle_overdue)
SELECT DISTINCT q.* FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]])
SELECT t.id AS "Номер", t.description AS "Описание", coalesce(c.name,'Компания неизвестна') AS "Компания",
 coalesce(d.name,'Отдел неизвестен') AS "Отдел отбора", coalesce(u.name,CASE WHEN s.actor_id=t.client_id THEN coalesce(nullif(t.client_name,''),'Клиент #'||s.actor_id) WHEN s.actor_id IS NOT NULL THEN 'Неизвестный участник #'||s.actor_id ELSE 'Не назначен / автор неизвестен' END) AS "Сотрудник отбора",
 CASE t.status WHEN 'new' THEN 'Новая' WHEN 'in_progress' THEN 'В работе' WHEN 'waiting_client' THEN 'Ожидает клиента' WHEN 'closed' THEN 'Закрыта' ELSE 'Отменена' END AS "Сейчас",
 t.created_at AT TIME ZONE 'Asia/Irkutsk' AS "Создана", s.at AT TIME ZONE 'Asia/Irkutsk' AS "Дата отбора",
 round(extract(epoch FROM (r.snapshot_at-t.created_at))/3600.0,2) AS "Возраст, кал. ч",
 round(t.first_response_work_seconds::numeric/60,2) AS "Первый ответ, раб. мин",
 round(t.response_waiting_work_seconds::numeric/60,2) AS "Без ответа, раб. мин",
 round(t.idle_work_seconds::numeric/3600,2) AS "Без инженера, раб. ч",
 round(t.waiting_work_seconds::numeric/3600,2) AS "Ожидание клиента, раб. ч",
 concat_ws('; ',CASE WHEN t.no_assignee THEN 'Без исполнителя' END,CASE WHEN t.response_overdue THEN 'Нет ответа дольше порога' END,CASE WHEN t.idle_overdue THEN 'Без инженера дольше порога' END,CASE WHEN t.waiting_overdue THEN 'Ожидание клиента дольше порога' END) AS "Причины внимания",
 CASE WHEN t.incomplete THEN t.quality ELSE 'Полные сведения' END AS "Качество истории"
FROM selected s JOIN analytics.tickets t ON t.id=s.ticket_id LEFT JOIN analytics.companies c ON c.id=t.company_id
LEFT JOIN analytics.departments d ON d.code=s.department LEFT JOIN analytics.employees u ON u.id=s.actor_id
CROSS JOIN analytics.refresh_state r WHERE r.id=1 ORDER BY t.created_at,t.id
