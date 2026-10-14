"""Task-oriented presentation; calculations and exact drilldown cohorts stay in build.py."""
import re
import json
from pathlib import Path

def configure(cards, add, bounds, closed, cohorts, filters):
    add('status', 'Свежесть данных', """SELECT
 CASE WHEN snapshot_at IS NULL THEN 'Нет снимка — показатели пока недоступны'
 WHEN last_error IS NOT NULL AND last_error<>'' THEN 'Ошибка обновления — показан предыдущий снимок'
 WHEN now()-snapshot_at>interval '5 minutes' THEN 'Данные устарели — проверьте обновление'
 ELSE 'Обновляется каждую минуту' END AS "Данные",
 to_char(snapshot_at AT TIME ZONE 'Asia/Irkutsk','DD.MM.YYYY HH24:MI') AS "Снимок · Иркутск",
 CASE WHEN incomplete_count>0 THEN 'Неполная история: '||incomplete_count||' заявок' ELSE 'Без выявленных пробелов' END AS "История"
 FROM analytics.refresh_state WHERE id=1""", width=24, height=3)
    add('attention_summary', 'Состояние очереди', """SELECT
 CASE WHEN count(*)=0 THEN 'Открытых заявок нет'
 WHEN count(*) FILTER(WHERE t.no_assignee OR t.response_overdue OR t.idle_overdue OR t.waiting_overdue)=0 THEN 'Заявок с признаками внимания нет'
 ELSE 'Проверьте заявки в списке ниже' END AS "Что происходит",
 CASE WHEN count(*)=0 THEN 'Новые обращения появятся после следующего обновления.'
 ELSE 'Начните с неназначенных и заявок без ответа. Нажмите номер, чтобы открыть историю.' END AS "Что делать"
 FROM analytics.tickets t WHERE t.status IN ('new','in_progress','waiting_client')
 [[ AND t.department={{department}} ]] [[ AND t.assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]""", width=24,height=3)
    # Reuse each cohort, including last closure BEFORE employee/department filtering.
    measures=[]
    for key,label in [('received','Поступило'),('closed','Закрыто'),('cancelled','Отменено'),('reopened','Открыто повторно')]:
        measures.append("SELECT '"+label+"' AS metric, count(*) AS n FROM (WITH p AS (SELECT w.lo,w.hi), "+closed+", q AS ("+cohorts[key]+") SELECT DISTINCT q.ticket_id FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true "+filters()+") selected")
    add('comparison','Изменения к предыдущему периоду', 'WITH '+bounds+""", windows AS (
 SELECT 'current' AS kind,lo,hi FROM p
 UNION ALL SELECT 'previous',lo-GREATEST(hi-lo,interval '0 seconds'),lo FROM p
), counts AS (
 SELECT w.kind,q.* FROM windows w CROSS JOIN LATERAL ("""+' UNION ALL '.join(measures)+""") q
)
SELECT c.metric AS "Показатель",c.n AS "За период",b.n AS "Ранее",c.n-b.n AS "Изменение"
FROM counts c JOIN counts b ON b.metric=c.metric AND b.kind='previous'
WHERE c.kind='current' ORDER BY CASE c.metric WHEN 'Поступило' THEN 1 WHEN 'Закрыто' THEN 2 WHEN 'Отменено' THEN 3 ELSE 4 END""",description='Ранее — интервал той же длительности непосредственно перед началом выбранного периода. Текущий период обрезан временем снимка. Изменение — разница в заявках, не оценка качества.',width=24,height=5)
    rows=[]
    for column,label in [('work_seconds','Решение · рабочие часы'),('calendar_seconds','Решение · календарные часы'),('waiting_work_seconds','Ожидание клиента · рабочие часы'),('work_without_waiting_seconds','Без ожидания · рабочие часы')]:
        rows.append(f"SELECT '{label}' AS \"Показатель\",percentile_cont(0.5) WITHIN GROUP(ORDER BY {column})/3600 AS \"Медиана\",avg({column})/3600 AS \"Среднее\",min({column})/3600 AS \"Минимум\",max({column})/3600 AS \"Максимум\",count({column}) AS \"Выборка\" FROM selected")
    add('resolution_summary','Сколько занимает решение','WITH '+bounds+', '+closed+", selected AS (SELECT c.* FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id LEFT JOIN analytics.cycles c ON c.ticket_id=q.ticket_id AND c.number=q.cycle_number WHERE true "+filters()+") "+' UNION ALL '.join(rows),width=24,height=6,target='closed_list')

    by_key={c['key']:c for c in cards}
    for key,name in {'received_count':'Поступило за период','closed_count':'Закрыто за период','active_now_count':'Открыто сейчас','attention_count':'Требуют внимания','unassigned_count':'Без исполнителя','response_overdue_count':'Ответ просрочен','idle_overdue_count':'Нет активности','waiting_overdue_count':'Клиент долго не отвечает','waiting_now_count':'Ждём клиента','unanswered_count':'Без первого ответа','cancelled_count':'Отменено за период','reopened_count':'Открыто повторно','backlog_count':'Очередь на конец периода','employees':'Результаты и нагрузка команды','flow':'Как меняется поток заявок'}.items():
        by_key[key]['name']=name
    for c in cards:
        c['settings']={}
        if c['display']=='scalar':
            c['settings']={'scalar.decimals':0}
        if c['display']=='table':
            c['settings']={'table.cell_column':False,'table.pivot':False}
    def columns(key, visible):
        names=list(dict.fromkeys(re.findall(r'AS "([^"]+)"', (Path(__file__).parent/by_key[key]['sql']).read_text())))
        by_key[key]['settings']['table.columns']=[{'name':n,'enabled':n in visible} for n in visible]+[{'name':n,'enabled':False} for n in names if n not in visible]
    columns('employees',['Сотрудник','Закрыл за период','Назначено сейчас','Ожидают клиента','Решение: медиана, раб. ч','Решение: выборка','Возвраты после его закрытий'])
    by_key['employees']['settings']['column_settings']={json.dumps(['name',column],ensure_ascii=False):{'column_title':label} for column,label in {'Закрыл за период':'Закрыто','Назначено сейчас':'В работе','Ожидают клиента':'Ждём клиента','Решение: медиана, раб. ч':'Медиана · раб. ч','Решение: выборка':'Выборка','Возвраты после его закрытий':'Возвраты'}.items()}
    for key in ('employees','resolution_summary','first_response'):
        settings=by_key[key]['settings'].setdefault('column_settings',{})
        sql=(Path(__file__).parent/by_key[key]['sql']).read_text()
        for name in re.findall(r'AS "([^"]+)"',sql):
            if any(part in name for part in ('Медиана','Среднее','Минимум','Максимум','раб. ч','раб. мин','кал. мин')):
                settings.setdefault(json.dumps(['name',name],ensure_ascii=False),{})['decimals']=2
    for c in cards:
        if c['key'].endswith('_list'):
            columns(c['key'], ['Номер','Описание','Компания','Отдел отбора','Сотрудник отбора','Сейчас']+(['Решение, раб. ч'] if c['key']=='closed_list' else [])+(['Причины внимания'] if c['key'] in ('attention_list','unassigned_list','response_overdue_list','idle_overdue_list','waiting_overdue_list') else [])) if c['key'] not in ('response_list','returns_list','transfer_list') else None
    list_names={'received':'Поступившие заявки','closed':'Закрытые заявки','cancelled':'Отменённые заявки','reopened':'Повторно открытые заявки','backlog':'Очередь на конец периода','active_now':'Открытые заявки','attention':'Заявки, требующие внимания','unassigned':'Заявки без исполнителя','unanswered':'Заявки без первого ответа','waiting_now':'Ждём ответа клиента','response_overdue':'Просроченный первый ответ','idle_overdue':'Заявки без активности инженера','waiting_overdue':'Длительное ожидание клиента','oldest':'Все открытые — сначала старые'}
    for key,name in list_names.items():
        by_key[key+'_list']['name']=name
    # Read a ticket like a document instead of scrolling a single 14-column row.
    ticket_path=Path(__file__).parent/by_key['ticket']['sql']
    original=ticket_path.read_text().strip()
    labels=re.findall(r'AS "([^"]+)"',original)
    fields=[]
    for i,label in enumerate(labels):
        value=f'd."{label}"::text'
        if label in ('Создана','Изменена','Текущий цикл','Первый ответ текущего цикла'):
            value=f"to_char(d.\"{label}\",'DD.MM.YYYY HH24:MI')"
        elif label=='Неполная история':
            value="CASE WHEN d.\"Неполная история\" THEN 'Да' ELSE 'Нет' END"
        elif label=='Ограничения':
            value="CASE WHEN d.\"Ограничения\"='complete' THEN 'Без выявленных пробелов' ELSE d.\"Ограничения\" END"
        fields.append(f"({i},'{label}',{value})")
    fields=','.join(fields)
    ticket_path.write_text('SELECT v.label AS "Поле",v.value AS "Значение" FROM ('+original+') d CROSS JOIN LATERAL (VALUES '+fields+') v(position,label,value) ORDER BY v.position\n')
    by_key['ticket']['height']=14
    columns('ticket_cycles',['Цикл','Начало','Завершение','Результат','Закрывший','Описание результата','Срок, раб. ч'])
    columns('ticket_history',['Дата','Событие','Автор','Отдел','Описание'])
    by_key['flow']['settings'].update({'graph.dimensions':['Дата'],'graph.metrics':['Поступило','Закрыто уникальных за день','Очередь на конец дня'],'graph.colors':['#509EE3','#2DAD8D','#A989C5'],'graph.y_axis.title_text':'Заявки','graph.x_axis.title_text':'Дата · Иркутск','graph.show_values':False,'graph.y_axis.auto_split':False})
    for key,dimension in [('received_department','Отдел'),('received_company','Компания')]:
        by_key[key]['display']='bar'
        by_key[key]['settings']={'graph.dimensions':[dimension],'graph.metrics':['Заявок'],'graph.colors':['#509EE3'],'graph.show_values':True}

    def text(key,content,height=2): return {'text':content,'key':key,'width':24,'height':height}
    def card(key,width=24,height=5): return {'card':key,'width':width,'height':height}
    intro=text('intro','**Нажмите число для списка заявок.** Без дат — текущий месяц. «Сейчас» — очередь на момент снимка.')
    return {
      'overview':[intro,card('received_count',6,3),card('closed_count',6,3),card('active_now_count',6,3),card('attention_count',6,3),card('flow',24,7),card('comparison',24,5),text('compare_help','**Как читать сравнение.** «Ранее» — такой же по длительности интервал перед началом периода, с учётом времени снимка. Разница показана в заявках; рост не всегда означает улучшение.'),card('received_department',12,6),card('received_company',12,6)],
      'attention':[text('attention_intro','**Где нужна помощь сейчас.** Даты периода не влияют на эту вкладку. Отдел, сотрудник и компания сужают текущую очередь. Одна заявка может иметь несколько причин.'),card('attention_summary',24,3),card('unassigned_count',6,3),card('response_overdue_count',6,3),card('idle_overdue_count',6,3),card('waiting_overdue_count',6,3),card('attention_list',24,8),text('queue_intro','**Вся открытая очередь.** Если список внимания пуст, проверьте общую нагрузку ниже.'),card('active_now_count',8,3),card('waiting_now_count',8,3),card('unanswered_count',8,3),card('oldest_list',24,8)],
      'employees':[text('team_intro','**Кто закрывает заявки и кому нужна помощь.** Закрытия — за период, нагрузка — сейчас. Нажмите число закрытий или нагрузки для списка. Срок решения — длительность цикла, а не трудозатраты сотрудника.',3),card('employees',24,9),card('first_response',24,5),text('team_help','**Без единого рейтинга.** Учитывайте сложность задач, размер выборки и ожидание клиента. Закрытие относится к автору события; первый ответ — к действительно ответившему сотруднику.')],
      'reports':[text('reports_intro','**Результаты за период.** Даты включаются целиком; текущий день ограничен временем снимка. Для точного списка нажмите показатель.'),card('received_count',6,3),card('closed_count',6,3),card('cancelled_count',6,3),card('reopened_count',6,3),card('resolution_summary',24,6),card('first_response',24,5),card('backlog_count',24,3),card('backlog_department',12,6),card('backlog_company',12,6)],
      'guide':[text('guide','### Как читать панель\n\n**Обзор** — главные числа и динамика. **Требуют внимания** — текущие проблемы. **Сотрудники** — результаты и нагрузка. **Подробно** — сроки и состав очереди.\n\nФильтры отдела, компании и сотрудника сохраняются при переходе из числа в список. Номер заявки открывает её историю. Назад к отбору — кнопкой браузера.\n\n**Период:** пустые даты означают текущий месяц по Иркутску. **Сейчас:** последний минутный снимок независимо от дат. Ноль означает отсутствие заявок; пустая длительность — отсутствие измерений.',7),text('definitions','### Что именно считаем\n\n**Закрыто:** каждая заявка один раз за период, по её последнему закрытию в этом периоде. Сумма дневных закрытий может отличаться из-за повторных циклов.\n\n**Сотрудник:** поступления — первый назначенный, закрытия — автор закрытия, текущая очередь — назначенный сейчас. Поэтому показатели одного сотрудника не обязаны сходиться в баланс.\n\n**Медиана:** половина измеренных сроков меньше этого значения. Рабочие часы учитывают календарь поддержки. Размер выборки указан рядом; срок включает ожидание клиента.\n\n**Качество истории:** отсутствие выявленных пробелов не гарантирует полноту старых событий. При предупреждении о задержке ориентируйтесь на время снимка.',8),card('freshness',24,4),card('operations',24,5),card('queue_balance',24,5)]
    }
