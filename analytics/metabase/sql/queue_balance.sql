WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), days AS (
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
FROM counts ORDER BY day
