#!/usr/bin/env python3
"""Deterministically build Russian OSS Metabase questions and dashboard manifest."""
import json
from pathlib import Path
ROOT = Path(__file__).resolve().parent
ACTIVE = "('new','in_progress','waiting_client')"
BOUNDS = """bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
)"""

def filters(dept='q.department', actor='q.actor_id', company='t.company_id'):
    return f"[[ AND {dept} = {{{{department}}}} ]]\n[[ AND {actor} = {{{{employee}}}} ]]\n[[ AND {company} = {{{{company}}}} ]]"

# Always select last closure BEFORE applying department/employee filters.
CLOSED = """last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
)"""
cohorts = {
 'received': "SELECT t.id AS ticket_id, e.department, t.first_assignee_id AS actor_id, t.created_at AS at FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true, p WHERE t.created_at>=p.lo AND t.created_at<p.hi",
 'closed': "SELECT ticket_id, department, actor_id, occurred_at AS at,cycle_number FROM last_closed",
 'cancelled': "SELECT DISTINCT ON (ticket_id) ticket_id,department,actor_id,occurred_at AS at FROM analytics.events,p WHERE kind='cancelled' AND occurred_at>=p.lo AND occurred_at<p.hi ORDER BY ticket_id,occurred_at DESC,id DESC",
 'reopened': "SELECT DISTINCT ON (ticket_id) ticket_id,department,actor_id,occurred_at AS at FROM analytics.events,p WHERE kind='reopened' AND occurred_at>=p.lo AND occurred_at<p.hi ORDER BY ticket_id,occurred_at DESC,id DESC",
 'backlog': f"SELECT ticket_id,department,assignee_id AS actor_id,p.hi AS at FROM analytics.state_intervals,p WHERE started_at<p.hi AND (ended_at IS NULL OR ended_at>=p.hi) AND status IN {ACTIVE}",
}
current = {
 'active_now': (f"t.status IN {ACTIVE}", 'Активно сейчас'),
 'unassigned': (f"t.status IN {ACTIVE} AND t.no_assignee", 'Сейчас: без исполнителя'),
 'unanswered': (f"t.status IN {ACTIVE} AND t.first_response_at IS NULL", 'Сейчас: без первого ответа'),
 'waiting_now': ("t.status='waiting_client'", 'Сейчас: ожидают клиента'),
 'response_overdue': (f"t.status IN {ACTIVE} AND t.response_overdue", 'Нет ответа дольше порога'),
 'idle_overdue': ("t.status='in_progress' AND t.idle_overdue", 'Без активности инженера дольше порога'),
 'waiting_overdue': ("t.status='waiting_client' AND t.waiting_overdue", 'Ожидание клиента дольше порога'),
 'attention': (f"t.status IN {ACTIVE} AND (t.no_assignee OR t.response_overdue OR t.idle_overdue OR t.waiting_overdue)", 'Сейчас: требуют внимания'),
 'oldest': (f"t.status IN {ACTIVE}", 'Самые старые активные заявки'),
}
for key,(pred,_) in current.items():
    cohorts[key] = f"SELECT t.id AS ticket_id,t.department,t.assignee_id AS actor_id,t.created_at AS at FROM analytics.tickets t WHERE {pred}"
NAMES={'received':'Поступило','closed':'Закрыто уникальных','cancelled':'Отменено уникальных','reopened':'Повторно открыто уникальных','backlog':'Активно на конец периода', **{k:v[1] for k,v in current.items()}}
DESCRIPTIONS={
 'received':'Дата первоначального создания. Сотрудник — первый назначенный; отдел — исторический при создании. Неизвестные значения не заменяются текущими.',
 'closed':'Последнее закрытие заявки внутри периода выбирается до фильтра отдела/сотрудника. Сотрудник — фактический автор этого закрытия.',
 'cancelled':'Уникальные заявки с отменой за период; сотрудник — автор последней отмены внутри периода.',
 'reopened':'Уникальные заявки с повторным открытием за период; сотрудник — автор последнего открытия внутри периода.',
 'backlog':'Состояние непосредственно перед исключённой правой границей периода. Сотрудник — назначенный тогда. Дата создания не ограничивается.',
}
cards=[]
def add(key,name,sql,display='table',description='',dashboard=None,tab=None,width=12,height=6,target=None):
    (ROOT/'sql'/f'{key}.sql').write_text(sql.strip()+'\n')
    cards.append(dict(key=key,name=name,sql=f'sql/{key}.sql',display=display,description=description,dashboard=dashboard,tab=tab,width=width,height=height,target=target))

for key,query in cohorts.items():
    prefix='WITH '+BOUNDS+', '+CLOSED+', q AS ('+query+')\n'
    cohort=prefix+'SELECT DISTINCT q.* FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true\n'+filters()
    add(key+'_count',NAMES[key],"WITH selected AS ("+cohort+") SELECT count(*) AS \"Заявок\" FROM selected",'scalar',DESCRIPTIONS.get(key,'Текущее состояние; сотрудник — текущий назначенный. Пороги рассчитаны календарём и конфигурацией бота. Даты периода не ограничивают этот блок.'),'support','overview',6,4,key+'_list')
    # List uses exactly the same cohort as its scalar, including historical dimensions.
    sql="WITH selected AS ("+cohort+")\n"+"""SELECT t.id AS "Номер", t.description AS "Описание", coalesce(c.name,'Компания неизвестна') AS "Компания",
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
CROSS JOIN analytics.refresh_state r WHERE r.id=1 ORDER BY t.created_at,t.id"""
    if key=='closed':
        sql=sql.replace('t.first_response_work_seconds::numeric','cc.first_response_work_seconds::numeric')
        sql=sql.replace('t.created_at AT TIME ZONE', 'cc.work_seconds/3600 AS "Решение, раб. ч",cc.calendar_seconds/3600 AS "Решение, кал. ч",cc.waiting_work_seconds/3600 AS "Ожидание цикла, раб. ч",cc.work_without_waiting_seconds/3600 AS "Без ожидания цикла, раб. ч", t.created_at AT TIME ZONE')
        sql=sql.replace('FROM selected s JOIN analytics.tickets t ON t.id=s.ticket_id', 'FROM selected s JOIN analytics.tickets t ON t.id=s.ticket_id LEFT JOIN analytics.cycles cc ON cc.ticket_id=s.ticket_id AND cc.number=s.cycle_number')
    add(key+'_list',NAMES[key]+' — заявки',sql,description=DESCRIPTIONS.get(key,''),dashboard='attention' if key in ['unassigned','response_overdue','idle_overdue','waiting_overdue','oldest'] else None,tab='attention',width=24,height=8,target='ticket')

add('freshness','Обновление и качество данных',"""SELECT snapshot_at AT TIME ZONE 'Asia/Irkutsk' AS "Снимок",completed_at AT TIME ZONE 'Asia/Irkutsk' AS "Обновлено",
 calculation_version AS "Версия расчёта",incomplete_count AS "Неполных заявок",
 CASE WHEN snapshot_at IS NULL THEN 'ВНИМАНИЕ: снимок ещё не рассчитан'
 WHEN now()-snapshot_at>interval '5 minutes' THEN 'ВНИМАНИЕ: данные старше пяти минут'
 WHEN last_error IS NOT NULL AND last_error<>'' THEN 'ВНИМАНИЕ: последнее обновление завершилось ошибкой' ELSE 'Данные актуальны' END AS "Состояние",
 last_attempt_at AT TIME ZONE 'Asia/Irkutsk' AS "Последняя попытка",last_error AS "Ошибка"
FROM analytics.refresh_state WHERE id=1""",dashboard='support',tab='overview',width=24,height=4)

add('flow','Поступления, закрытия и очередь по дням','WITH '+BOUNDS+""", days AS (SELECT generate_series(start_day::timestamp,LEAST(end_day,(SELECT (snapshot_at AT TIME ZONE 'Asia/Irkutsk')::date FROM analytics.refresh_state WHERE id=1))::timestamp,interval '1 day')::date AS day FROM bounds)
SELECT day AS "Дата",
 (SELECT count(*) FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true
 WHERE t.created_at>=day::timestamp AT TIME ZONE 'Asia/Irkutsk' AND t.created_at<(day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk'
 [[ AND e.department={{department}} ]] [[ AND t.first_assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]) AS "Поступило",
 (SELECT count(*) FROM (SELECT DISTINCT ON(ticket_id) * FROM analytics.events WHERE kind='closed' AND occurred_at>=day::timestamp AT TIME ZONE 'Asia/Irkutsk' AND occurred_at<(day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk' ORDER BY ticket_id,occurred_at DESC,id DESC) q
 JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
"""+filters()+"""
 ) AS "Закрыто уникальных за день",
 (SELECT count(*) FROM analytics.state_intervals q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.started_at<LEAST((day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk',(SELECT snapshot_at FROM analytics.refresh_state WHERE id=1)) AND (q.ended_at IS NULL OR q.ended_at>=LEAST((day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk',(SELECT snapshot_at FROM analytics.refresh_state WHERE id=1))) AND q.status IN ('new','in_progress','waiting_client')
"""+filters(actor='q.assignee_id')+"""
 ) AS "Очередь на конец дня" FROM days ORDER BY day""",display='line',description='Сумма дневных уникальных закрытий может отличаться от уникальных закрытий за весь период: повторные циклы одной заявки.',dashboard='support',tab='overview',width=24,height=8)

for dimension,label in [('department','Отдел'),('company','Компания')]:
    cohort='WITH '+BOUNDS+', '+CLOSED+', q AS ('+cohorts['received']+')\n'
    group="coalesce(d.name,'Отдел неизвестен')" if dimension=='department' else "coalesce(c.name,'Компания неизвестна')"
    joins='LEFT JOIN analytics.departments d ON d.code=q.department LEFT JOIN analytics.companies c ON c.id=t.company_id'
    add('received_'+dimension,'Поступления по '+('отделам' if dimension=='department' else 'компаниям'),cohort+f'SELECT {group} AS "{label}",count(*) AS "Заявок" FROM q JOIN analytics.tickets t ON t.id=q.ticket_id '+joins+' WHERE true\n'+filters()+f' GROUP BY {group} ORDER BY count(*) DESC',dashboard='support',tab='overview',width=12,height=6)
    cohort='WITH '+BOUNDS+', '+CLOSED+', q AS ('+cohorts['backlog']+')\n'
    add('backlog_'+dimension,'Очередь на конец периода по '+('отделам' if dimension=='department' else 'компаниям'),cohort+f'SELECT {group} AS "{label}",count(*) AS "Заявок" FROM q JOIN analytics.tickets t ON t.id=q.ticket_id '+joins+' WHERE true\n'+filters()+f' GROUP BY {group} ORDER BY count(*) DESC',dashboard='support',tab='overview',width=12,height=6)

stats=[]
for col,label,div in [('calendar_seconds','Решение, кал. ч',3600),('work_seconds','Решение, раб. ч',3600),('work_without_waiting_seconds','Без ожидания, раб. ч',3600),('waiting_work_seconds','Ожидание, раб. ч',3600)]:
    for expr,stat in [(f'count(c.{col})','выборка'),(f'avg(c.{col})/{div}','среднее'),(f'percentile_cont(0.5) WITHIN GROUP (ORDER BY c.{col})/{div}','медиана'),(f'min(c.{col})/{div}','мин'),(f'max(c.{col})/{div}','макс')]:
        stats.append(f'{expr} AS "{label}: {stat}"')
add('resolution','Срок решения последнего закрытия за период','WITH '+BOUNDS+', '+CLOSED+' SELECT '+',\n'.join(stats)+' FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.cycles c ON c.ticket_id=q.ticket_id AND c.number=q.cycle_number WHERE true\n'+filters(),description='Продолжительность цикла включает ожидание клиента. Это возраст цикла, а не личные трудозатраты автора закрытия. NULL исключены из статистики, выборка рядом.',dashboard='support',tab='overview',width=24,height=6)
add('first_response','Первый подтверждённый ответ за период','WITH '+BOUNDS+""" SELECT count(c.first_response_work_seconds) AS "Выборка",
 avg(c.first_response_work_seconds)/60 AS "Среднее, раб. мин",
 percentile_cont(0.5) WITHIN GROUP(ORDER BY c.first_response_work_seconds)/60 AS "Медиана, раб. мин",
 avg(c.first_response_seconds)/60 AS "Среднее, кал. мин"
FROM analytics.cycles c JOIN analytics.tickets t ON t.id=c.ticket_id,p
WHERE c.first_response_at>=p.lo AND c.first_response_at<p.hi
[[ AND c.first_response_department={{department}} ]] [[ AND c.first_response_by={{employee}} ]] [[ AND t.company_id={{company}} ]]""",description='Выборка по дате доставки ответа в цикле. Сотрудник — фактический автор первого подтверждённого ответа. NULL не равен нулю.',dashboard='support',tab='overview',width=24,height=5)
add('operations','Операции за период (не уникальные заявки)','WITH '+BOUNDS+" SELECT CASE q.kind WHEN 'closed' THEN 'Закрытия' WHEN 'cancelled' THEN 'Отмены' WHEN 'reopened' THEN 'Повторные открытия' WHEN 'transferred' THEN 'Передачи' END AS \"Событие\",count(*) AS \"Операций\" FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id,p WHERE q.kind IN ('closed','cancelled','reopened','transferred') AND q.occurred_at>=p.lo AND q.occurred_at<p.hi\n"+filters()+" GROUP BY q.kind ORDER BY q.kind",dashboard='support',tab='overview',width=24,height=5)

add('response_list','Первые ответы — циклы','WITH '+BOUNDS+""" SELECT t.id AS "Номер",c.number AS "Цикл",c.first_response_at AT TIME ZONE 'Asia/Irkutsk' AS "Ответ доставлен",coalesce(u.name,'Участник #'||c.first_response_by) AS "Ответил",d.name AS "Отдел ответа",co.name AS "Компания",c.first_response_seconds/60 AS "Первый ответ, кал. мин",c.first_response_work_seconds/60 AS "Первый ответ, раб. мин",c.incomplete AS "Неполная история"
FROM analytics.cycles c JOIN analytics.tickets t ON t.id=c.ticket_id LEFT JOIN analytics.employees u ON u.id=c.first_response_by LEFT JOIN analytics.departments d ON d.code=c.first_response_department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE c.first_response_at>=p.lo AND c.first_response_at<p.hi [[ AND c.first_response_department={{department}} ]] [[ AND c.first_response_by={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY c.first_response_at,t.id,c.number""",target='ticket')
add('transfer_list','Передачи — операции','WITH '+BOUNDS+""" SELECT t.id AS "Номер",q.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Передана",u.name AS "Передал",f.name AS "Из отдела",d.name AS "В отдел",co.name AS "Компания",q.detail AS "Причина",q.incomplete AS "Неполная история"
FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.employees u ON u.id=q.actor_id LEFT JOIN analytics.departments f ON f.code=q.from_department LEFT JOIN analytics.departments d ON d.code=q.to_department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE q.kind='transferred' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND q.from_department={{department}} ]] [[ AND q.actor_id={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY q.occurred_at,q.id""",target='ticket')
add('returns_list','Возвраты после закрытий сотрудника','WITH '+BOUNDS+""" SELECT t.id AS "Номер",q.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Повторно открыта",u.name AS "Предыдущий закрывший",prev.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Предыдущее закрытие",d.name AS "Отдел закрытия",co.name AS "Компания"
FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id
JOIN LATERAL (SELECT actor_id,department,occurred_at FROM analytics.events e WHERE e.ticket_id=q.ticket_id AND e.kind='closed' AND (e.occurred_at,e.id)<(q.occurred_at,q.id) ORDER BY e.occurred_at DESC,e.id DESC LIMIT 1) prev ON true
LEFT JOIN analytics.employees u ON u.id=prev.actor_id LEFT JOIN analytics.departments d ON d.code=prev.department LEFT JOIN analytics.companies co ON co.id=t.company_id,p
WHERE q.kind='reopened' AND q.occurred_at>=p.lo AND q.occurred_at<p.hi [[ AND prev.department={{department}} ]] [[ AND prev.actor_id={{employee}} ]] [[ AND t.company_id={{company}} ]] ORDER BY q.occurred_at,q.id""",target='ticket')

add('queue_balance','Проверка баланса очереди','WITH '+BOUNDS+""", days AS (
 SELECT day::date, day AT TIME ZONE 'Asia/Irkutsk' AS lo,
 LEAST((day+interval '1 day') AT TIME ZONE 'Asia/Irkutsk',r.snapshot_at) AS hi
 FROM bounds CROSS JOIN analytics.refresh_state r,
 LATERAL generate_series(start_day::timestamp,LEAST(end_day,(r.snapshot_at AT TIME ZONE 'Asia/Irkutsk')::date)::timestamp,interval '1 day') day WHERE r.id=1
), counts AS (
 SELECT day,
 (SELECT count(*) FROM analytics.state_intervals q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.started_at<days.lo AND (q.ended_at IS NULL OR q.ended_at>=days.lo) AND q.status IN ('new','in_progress','waiting_client') [[ AND q.department={{department}} ]] [[ AND t.company_id={{company}} ]]) AS opening,
 (SELECT count(*) FROM analytics.state_intervals q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.started_at<days.hi AND (q.ended_at IS NULL OR q.ended_at>=days.hi) AND q.status IN ('new','in_progress','waiting_client') [[ AND q.department={{department}} ]] [[ AND t.company_id={{company}} ]]) AS closing,
 (SELECT coalesce(sum(CASE WHEN q.kind IN ('created','reopened') THEN 1 WHEN q.kind IN ('closed','cancelled') THEN -1 ELSE 0 END),0) FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.occurred_at>=days.lo AND q.occurred_at<days.hi [[ AND q.department={{department}} ]] [[ AND t.company_id={{company}} ]]) AS operations,
 (SELECT count(*) FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.kind='transferred' AND q.occurred_at>=days.lo AND q.occurred_at<days.hi [[ AND q.to_department={{department}} ]] [[ AND t.company_id={{company}} ]]) AS incoming,
 (SELECT count(*) FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE q.kind='transferred' AND q.occurred_at>=days.lo AND q.occurred_at<days.hi [[ AND q.from_department={{department}} ]] [[ AND t.company_id={{company}} ]]) AS outgoing,
 (SELECT count(*) FROM analytics.tickets t WHERE t.incomplete [[ AND t.company_id={{company}} ]]) AS gaps
 FROM days
)
SELECT day AS "Дата",opening AS "Очередь на начало",closing AS "Очередь на конец",operations AS "Поступления + открытия − закрытия − отмены",incoming AS "Передачи входящие",outgoing AS "Передачи исходящие",closing-opening-operations-incoming+outgoing AS "Расхождение (должно быть 0)",gaps AS "Заявок с пробелами истории"
FROM counts ORDER BY day""",description='Проверка операций и состояний по компании/отделу. Фильтр сотрудника не применяется: автор закрытия отличается от назначенного. Входящие/исходящие передачи компенсируются при всех отделах. Неполная история может давать расхождение.',dashboard='support',tab='overview',width=24,height=7)

add('employees','Работа сотрудников','WITH '+BOUNDS+', '+CLOSED+""", closures AS (
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
WHERE true [[ AND u.id={{employee}} ]] ORDER BY coalesce(c.n,0) DESC,u.name""",description='Закрытия: автор последнего закрытия внутри периода. Нагрузка: назначенный сейчас. Ответ: фактический автор доставки. Возвраты: предыдущий закрывший. Длительность цикла не равна трудозатратам. Исторический отдел событий и циклов; текущие отделы показаны только для справки.',dashboard='support',tab='employees',width=24,height=12)

add('ticket','Карточка заявки',"""SELECT t.id AS "Номер",t.description AS "Описание",t.client_name AS "Клиент",c.name AS "Компания",CASE t.status WHEN 'new' THEN 'Новая' WHEN 'in_progress' THEN 'В работе' WHEN 'waiting_client' THEN 'Ожидает клиента' WHEN 'closed' THEN 'Закрыта' WHEN 'cancelled' THEN 'Отменена' ELSE 'Нет данных' END AS "Статус",d.name AS "Текущий отдел",u.name AS "Исполнитель",
 t.created_at AT TIME ZONE 'Asia/Irkutsk' AS "Создана",t.updated_at AT TIME ZONE 'Asia/Irkutsk' AS "Изменена",
 t.cycle_started_at AT TIME ZONE 'Asia/Irkutsk' AS "Текущий цикл",t.first_response_at AT TIME ZONE 'Asia/Irkutsk' AS "Первый ответ текущего цикла",
 round(extract(epoch FROM (r.snapshot_at-t.created_at))/3600.0,2) AS "Полный возраст, кал. ч",t.incomplete AS "Неполная история",t.quality AS "Ограничения"
FROM analytics.tickets t LEFT JOIN analytics.companies c ON c.id=t.company_id LEFT JOIN analytics.departments d ON d.code=t.department LEFT JOIN analytics.employees u ON u.id=t.assignee_id CROSS JOIN analytics.refresh_state r
WHERE r.id=1 AND t.id={{ticket_id}}""",dashboard='ticket',width=24,height=8)
add('ticket_cycles','Циклы и результаты закрытий',"""SELECT number AS "Цикл",started_at AT TIME ZONE 'Asia/Irkutsk' AS "Начало",ended_at AT TIME ZONE 'Asia/Irkutsk' AS "Завершение",CASE outcome WHEN 'active' THEN 'Активен' WHEN 'closed' THEN 'Закрыт' WHEN 'cancelled' THEN 'Отменён' ELSE 'Нет данных' END AS "Результат",u.name AS "Закрывший",d.name AS "Отдел",resolution AS "Описание результата",
 calendar_seconds/3600 AS "Срок, кал. ч",work_seconds/3600 AS "Срок, раб. ч",waiting_work_seconds/3600 AS "Ожидание, раб. ч",work_without_waiting_seconds/3600 AS "Без ожидания, раб. ч",first_response_work_seconds/60 AS "Первый ответ, раб. мин",r.name AS "Автор ответа",c.incomplete AS "Неполная история"
FROM analytics.cycles c LEFT JOIN analytics.employees u ON u.id=c.closed_by LEFT JOIN analytics.employees r ON r.id=c.first_response_by LEFT JOIN analytics.departments d ON d.code=c.department WHERE ticket_id={{ticket_id}} ORDER BY number""",dashboard='ticket',width=24,height=8)
add('ticket_history','История заявки',"""SELECT q.id AS "Событие №",q.occurred_at AT TIME ZONE 'Asia/Irkutsk' AS "Дата",CASE q.kind WHEN 'created' THEN 'Создана' WHEN 'claimed' THEN 'Взята в работу' WHEN 'reassigned' THEN 'Сменён исполнитель' WHEN 'transferred' THEN 'Передана' WHEN 'reopened' THEN 'Повторно открыта' WHEN 'status_changed' THEN 'Изменён статус' WHEN 'closed' THEN 'Закрыта' WHEN 'cancelled' THEN 'Отменена' WHEN 'message_delivered' THEN 'Доставка сообщения' WHEN 'message' THEN 'Сообщение' WHEN 'first_response' THEN 'Первый ответ' ELSE q.kind END AS "Событие",coalesce(u.name,CASE WHEN q.actor_id=t.client_id THEN nullif(t.client_name,'') WHEN q.actor_id IS NOT NULL THEN 'Участник #'||q.actor_id ELSE 'Нет данных' END) AS "Автор",d.name AS "Отдел",f.name AS "Откуда",dest.name AS "Куда",a.name AS "Назначенный",q.status AS "Статус",q.cycle_number AS "Цикл",q.detail AS "Описание",q.incomplete AS "Неполные сведения"
FROM analytics.events q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.employees u ON u.id=q.actor_id LEFT JOIN analytics.employees a ON a.id=q.assignee_id LEFT JOIN analytics.departments d ON d.code=q.department LEFT JOIN analytics.departments f ON f.code=q.from_department LEFT JOIN analytics.departments dest ON dest.code=q.to_department WHERE q.ticket_id={{ticket_id}} ORDER BY q.occurred_at,q.id""",dashboard='ticket',width=24,height=12)

add('filter_departments','Отделы для фильтра',"SELECT code AS value,name AS label FROM analytics.departments ORDER BY name")
add('filter_employees','Сотрудники для фильтра',"SELECT id AS value,name AS label FROM analytics.employees WHERE role IN ('engineer','admin','dispatcher') ORDER BY name,id")
add('filter_companies','Компании для фильтра',"SELECT id AS value,name AS label FROM analytics.companies ORDER BY name,id")

from presentation import configure
layout=configure(cards,add,BOUNDS,CLOSED,cohorts,filters)
manifest={'format':2,'compatibility_version':'v0.64.1','collection':'BVK — аналитика поддержки','dashboards':{'support':{'name':'Поддержка BVK','tabs':{'overview':'Обзор','attention':'Требуют внимания','employees':'Сотрудники','reports':'Подробно','guide':'Как читать'},'layout':layout},'ticket':{'name':'Карточка заявки','tabs':{}}},'cards':cards}
for c in cards:
    if c['key']=='resolution': c['target']='closed_list'
    if c['key']=='first_response': c['target']='response_list'
(ROOT/'manifest.json').write_text(json.dumps(manifest,ensure_ascii=False,indent=2)+'\n')
print(f'Generated {len(cards)} cards')
