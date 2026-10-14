SELECT
 CASE WHEN count(*)=0 THEN 'Открытых заявок нет'
 WHEN count(*) FILTER(WHERE t.no_assignee OR t.response_overdue OR t.idle_overdue OR t.waiting_overdue)=0 THEN 'Заявок с признаками внимания нет'
 ELSE 'Проверьте заявки в списке ниже' END AS "Что происходит",
 CASE WHEN count(*)=0 THEN 'Новые обращения появятся после следующего обновления.'
 ELSE 'Начните с неназначенных и заявок без ответа. Нажмите номер, чтобы открыть историю.' END AS "Что делать"
 FROM analytics.tickets t WHERE t.status IN ('new','in_progress','waiting_client')
 [[ AND t.department={{department}} ]] [[ AND t.assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]
