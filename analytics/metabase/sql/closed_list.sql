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
), q AS (SELECT ticket_id, department, actor_id, occurred_at AS at,cycle_number FROM last_closed)
SELECT DISTINCT q.* FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]
[[ AND q.at >= {{row_start}}::date::timestamp AT TIME ZONE 'Asia/Irkutsk' ]]
[[ AND q.at < ({{row_end}}::date + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk' ]])
SELECT t.id AS "Номер", t.description AS "Описание", coalesce(c.name,'Компания неизвестна') AS "Компания",
 coalesce(d.name,'Отдел неизвестен') AS "Отдел отбора", coalesce(u.name,CASE WHEN s.actor_id=t.client_id THEN coalesce(nullif(t.client_name,''),'Клиент #'||s.actor_id) WHEN s.actor_id IS NOT NULL THEN 'Неизвестный участник #'||s.actor_id ELSE 'Не назначен / автор неизвестен' END) AS "Сотрудник отбора",
 CASE t.status WHEN 'new' THEN 'Новая' WHEN 'in_progress' THEN 'В работе' WHEN 'waiting_client' THEN 'Ожидает клиента' WHEN 'closed' THEN 'Закрыта' ELSE 'Отменена' END AS "Сейчас",
 coalesce(CASE WHEN CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END IS NULL THEN NULL WHEN CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END<60 THEN '< 1 мин' WHEN floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint<60 THEN floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint||' мин' ELSE (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint / 60)||' '||CASE WHEN (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint / 60)%10=1 AND (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint / 60)%100<>11 THEN 'час' WHEN (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint / 60)%10 BETWEEN 2 AND 4 AND (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint / 60)%100 NOT BETWEEN 12 AND 14 THEN 'часа' ELSE 'часов' END ||CASE WHEN (floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint % 60)=0 THEN '' ELSE ' '||(floor((CASE WHEN NOT t.incomplete AND s.at>=t.created_at THEN EXTRACT(EPOCH FROM (s.at-t.created_at)) END) / 60)::bigint % 60)||' мин' END END,'—') AS "Срок от создания", t.created_at AT TIME ZONE 'Asia/Irkutsk' AS "Создана", s.at AT TIME ZONE 'Asia/Irkutsk' AS "Дата отбора",
 round(extract(epoch FROM (r.snapshot_at-t.created_at))/3600.0,2) AS "Возраст, кал. ч",
 round(cc.first_response_work_seconds::numeric/60,2) AS "Первый ответ, раб. мин",
 round(t.response_waiting_work_seconds::numeric/60,2) AS "Без ответа, раб. мин",
 round(t.idle_work_seconds::numeric/3600,2) AS "Без инженера, раб. ч",
 round(t.waiting_work_seconds::numeric/3600,2) AS "Ожидание клиента, раб. ч",
 concat_ws('; ',CASE WHEN t.no_assignee THEN 'Без исполнителя' END,CASE WHEN t.response_overdue THEN 'Нет ответа дольше порога' END,CASE WHEN t.idle_overdue THEN 'Без инженера дольше порога' END) AS "Причины внимания",
 CASE WHEN t.incomplete THEN t.quality ELSE 'Полные сведения' END AS "Качество истории"
FROM selected s JOIN analytics.tickets t ON t.id=s.ticket_id LEFT JOIN analytics.cycles cc ON cc.ticket_id=s.ticket_id AND cc.number=s.cycle_number LEFT JOIN analytics.companies c ON c.id=t.company_id
LEFT JOIN analytics.departments d ON d.code=s.department LEFT JOIN analytics.employees u ON u.id=s.actor_id
CROSS JOIN analytics.refresh_state r WHERE r.id=1 ORDER BY t.created_at,t.id
