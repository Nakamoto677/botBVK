"""Task-oriented presentation; calculations and exact drilldown cohorts stay in build.py."""
import re
import json
from pathlib import Path

def configure(cards, add, bounds, closed, cohorts, filters, duration):
    add('attention_summary', 'Состояние очереди', """SELECT
 CASE WHEN count(*)=0 THEN 'Открытых заявок нет'
 WHEN count(*) FILTER(WHERE t.no_assignee OR t.response_overdue OR t.idle_overdue)=0 THEN 'Заявок с признаками внимания нет'
 ELSE 'Проверьте заявки в списке ниже' END AS "Что происходит",
 CASE WHEN count(*)=0 THEN 'Новые обращения появятся после следующего обновления.'
 ELSE 'Начните с неназначенных и заявок без ответа. Нажмите номер, чтобы открыть историю.' END AS "Что делать"
 FROM analytics.tickets t WHERE t.status IN ('new','in_progress','waiting_client')
 [[ AND t.department={{department}} ]] [[ AND t.assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]""", width=24,height=3)
    # Reuse each cohort, including last closure BEFORE employee/department filtering.
    measures=[]
    for key,label in [('received','Поступило'),('closed','Закрыто'),('cancelled','Отменено')]:
        measures.append("SELECT '"+label+"' AS metric, count(*) AS n FROM (WITH p AS (SELECT w.lo,w.hi), "+closed+", q AS ("+cohorts[key]+") SELECT DISTINCT q.ticket_id FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true "+filters()+") selected")
    add('comparison','Изменения к предыдущему периоду', 'WITH '+bounds+""", windows AS (
 SELECT 'current' AS kind,lo,hi FROM p
 UNION ALL SELECT 'previous',lo-GREATEST(hi-lo,interval '0 seconds'),lo FROM p
), counts AS (
 SELECT w.kind,q.* FROM windows w CROSS JOIN LATERAL ("""+' UNION ALL '.join(measures)+""") q
)
SELECT c.metric AS "Показатель",c.n AS "За период",b.n AS "Ранее",c.n-b.n AS "Изменение"
FROM counts c JOIN counts b ON b.metric=c.metric AND b.kind='previous'
WHERE c.kind='current' ORDER BY CASE c.metric WHEN 'Поступило' THEN 1 WHEN 'Закрыто' THEN 2 ELSE 3 END""",description='Ранее — интервал той же длительности непосредственно перед началом выбранного периода. Текущий период обрезан временем снимка. Изменение — разница в заявках, не оценка качества.',width=24,height=5)
    add('resolution_summary','Сколько занимает выполнение','WITH '+bounds+', '+closed+", selected AS (SELECT CASE WHEN NOT t.incomplete AND q.occurred_at>=t.created_at THEN EXTRACT(EPOCH FROM (q.occurred_at-t.created_at)) END AS seconds FROM last_closed q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true "+filters()+"), totals AS (SELECT avg(seconds) AS average,percentile_cont(0.5) WITHIN GROUP(ORDER BY seconds) AS median,max(seconds) AS maximum,count(seconds) AS known FROM selected) SELECT coalesce("+duration('average')+",'—') AS \"Среднее время\",coalesce("+duration('median')+",'—') AS \"Медианное время\",coalesce("+duration('maximum')+",'—') AS \"Максимальное время\",known AS \"Измеренных сроков\" FROM totals",width=24,height=4,target='closed_list')

    by_key={c['key']:c for c in cards}
    for key,name in {'received_count':'Поступило за период','closed_count':'Закрыто за период','active_now_count':'Открыто сейчас','attention_count':'Требуют внимания','unassigned_count':'Без исполнителя','response_overdue_count':'Ответ просрочен','idle_overdue_count':'Нет активности','waiting_overdue_count':'Клиент долго не отвечает','waiting_now_count':'Ждём клиента','unanswered_count':'Без первого ответа','cancelled_count':'Отменено за период','reopened_count':'Открыто повторно','backlog_count':'Очередь на конец периода','employees':'Результаты и нагрузка команды','flow':'Результаты по периодам'}.items():
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
    columns('employees',['Сотрудник','Закрыто за период','В работе сейчас','Среднее время выполнения','Медианное время выполнения','Максимальное время выполнения'])
    columns('resolution_summary',['Среднее время','Медианное время','Максимальное время','Измеренных сроков'])
    columns('flow',['Дата / период','Поступило','Закрыто','Отменено','Открытые'])
    for c in cards:
        key=c['key']
        if not key.endswith('_list') or key in ('response_list','returns_list','transfer_list'):
            continue
        visible=['Номер']
        if key!='oldest_list':
            visible.append('Описание')
        visible+=['Компания','Отдел отбора','Сотрудник отбора','Сейчас']
        if key=='oldest_list':
            visible.append('Ожидает')
        if key=='closed_list':
            visible.append('Срок от создания')
        if key in ('attention_list','unassigned_list','response_overdue_list','idle_overdue_list'):
            visible.append('Причины внимания')
        columns(key,visible)
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
    for key,dimension in [('received_department','Отдел'),('received_company','Компания')]:
        by_key[key]['display']='bar'
        by_key[key]['settings']={'graph.dimensions':[dimension],'graph.metrics':['Заявок'],'graph.colors':['#509EE3'],'graph.show_values':True}

    def text(key,content,height=2): return {'text':content,'key':key,'width':24,'height':height}
    def card(key,width=24,height=5): return {'card':key,'width':width,'height':height}
    intro=text('intro','**Нажмите число для списка заявок.** Без дат — текущий месяц. «Сейчас» — очередь на момент снимка.')
    return {
      'overview':[intro,card('received_count',6,3),card('closed_count',6,3),card('cancelled_count',6,3),card('active_now_count',6,3),text('flow_help','**Результаты по периодам.** Поступило, закрыто и отменено — за строку; их суммы равны числам выше. Открытые — остаток незавершённых на конец строки, его не суммируют.'),card('flow',20,5),card('comparison',24,5),text('compare_help','**Ранее** — равный по длительности интервал перед выбранным периодом. Изменение показано в заявках.'),card('received_department',12,6),card('received_company',24,9)],
      'attention':[text('attention_intro','**Где нужна помощь сейчас.** Даты не влияют на вкладку. У одной заявки может быть несколько причин.'),card('attention_summary',24,3),card('unassigned_count',8,3),card('response_overdue_count',8,3),card('idle_overdue_count',8,3),card('attention_list',24,8),text('queue_intro','**Вся открытая очередь.** Если список внимания пуст, проверьте общую нагрузку ниже.'),card('active_now_count',12,3),card('unanswered_count',12,3),card('oldest_list',24,8)],
      'employees':[text('team_intro','**Закрытия — за период; нагрузка — сейчас.** Время — от назначения до закрытия, включая ожидание клиента.'),card('employees',24,9),text('team_help','Без достоверного назначения закрытие учитывается без срока.')],
      'reports':[text('reports_intro','**Результаты за период.** Даты включаются целиком; текущий день ограничен временем снимка. Для точного списка нажмите показатель.'),card('received_count',8,3),card('closed_count',8,3),card('cancelled_count',8,3),card('resolution_summary',24,4),card('backlog_count',24,3),card('backlog_department',12,6),card('backlog_company',12,6)],
      'guide':[text('guide','### Как читать панель\n\n**Обзор** — главные числа и результаты по дням, неделям или месяцам. **Требуют внимания** — текущие проблемы. **Сотрудники** — результаты и нагрузка. **Подробно** — сроки и состав очереди.\n\nФильтры отдела, компании и сотрудника сохраняются при переходе из числа в список. Номер заявки открывает её историю.\n\n**Период:** пустые даты означают текущий месяц по Иркутску. **Сейчас:** последний минутный снимок независимо от дат.',6),text('definitions','### Что именно считаем\n\n**Закрыто и отменено:** каждая заявка один раз за период, по последнему соответствующему событию внутри периода. **Открытые в строке периода:** незавершённые на её конец, включая не назначенные и ожидающие клиента.\n\n**Сотрудник:** закрытия — автор события, текущая очередь — назначенный сейчас. Срок сотрудника — от его последнего назначения до закрытия; без достоверного назначения срок не измеряется.\n\n**Время выполнения:** от создания заявки до закрытия, непрерывно, с ожиданием клиента, ночами и выходными. Среднее, медиану и максимум считают только по известным срокам.',7),card('operations',24,5),card('queue_balance',24,5)]
    }
