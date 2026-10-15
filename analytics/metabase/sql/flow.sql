WITH bounds AS (
 SELECT COALESCE([[{{start_date}}::date,]] date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk')::date) AS start_day,
 COALESCE([[{{end_date}}::date,]] (date_trunc('month', now() AT TIME ZONE 'Asia/Irkutsk') + interval '1 month - 1 day')::date) AS end_day
), p AS (
 SELECT start_day::timestamp AT TIME ZONE 'Asia/Irkutsk' AS lo,
 CASE WHEN r.snapshot_at IS NOT NULL THEN LEAST((end_day + 1)::timestamp AT TIME ZONE 'Asia/Irkutsk', r.snapshot_at) END AS hi FROM bounds CROSS JOIN analytics.refresh_state r WHERE r.id=1
), span AS (
 SELECT (lo AT TIME ZONE 'Asia/Irkutsk')::date AS first_day,
 CASE WHEN hi IS NOT NULL THEN ((hi-interval '1 microsecond') AT TIME ZONE 'Asia/Irkutsk')::date END AS final_day,
 CASE WHEN hi IS NOT NULL THEN ((hi-interval '1 microsecond') AT TIME ZONE 'Asia/Irkutsk')::date-(lo AT TIME ZONE 'Asia/Irkutsk')::date+1 END AS days,
 hi < (b.end_day+1)::timestamp AT TIME ZONE 'Asia/Irkutsk' AS partial,
 hi IS NULL AS unavailable
 FROM p CROSS JOIN bounds b
), events AS (
 SELECT kind,at FROM p outer_p CROSS JOIN LATERAL (SELECT 'received' AS kind,q.at FROM (WITH p AS (SELECT outer_p.lo,outer_p.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT t.id AS ticket_id, e.department, t.first_assignee_id AS actor_id, t.created_at AS at FROM analytics.tickets t LEFT JOIN LATERAL (SELECT department FROM analytics.events WHERE ticket_id=t.id AND kind='created' ORDER BY occurred_at,id LIMIT 1) e ON true, p WHERE t.created_at>=p.lo AND t.created_at<p.hi) SELECT q.at FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) q UNION ALL SELECT 'closed' AS kind,q.at FROM (WITH p AS (SELECT outer_p.lo,outer_p.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT ticket_id, department, actor_id, occurred_at AS at,cycle_number FROM last_closed) SELECT q.at FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) q UNION ALL SELECT 'cancelled' AS kind,q.at FROM (WITH p AS (SELECT outer_p.lo,outer_p.hi), last_closed AS (
 SELECT DISTINCT ON (ticket_id) * FROM analytics.events, p
 WHERE kind='closed' AND occurred_at >= p.lo AND occurred_at < p.hi
 ORDER BY ticket_id, occurred_at DESC, id DESC
), q AS (SELECT DISTINCT ON (ticket_id) ticket_id,department,actor_id,occurred_at AS at FROM analytics.events,p WHERE kind='cancelled' AND occurred_at>=p.lo AND occurred_at<p.hi ORDER BY ticket_id,occurred_at DESC,id DESC) SELECT q.at FROM q JOIN analytics.tickets t ON t.id=q.ticket_id WHERE true
[[ AND q.department = {{department}} ]]
[[ AND q.actor_id = {{employee}} ]]
[[ AND t.company_id = {{company}} ]]) q) source
), grouped AS (
 SELECT kind,CASE WHEN s.days<=31 THEN (at AT TIME ZONE 'Asia/Irkutsk')::date
 WHEN s.days<=90 THEN date_trunc('week',at AT TIME ZONE 'Asia/Irkutsk')::date
 ELSE date_trunc('month',at AT TIME ZONE 'Asia/Irkutsk')::date END AS bucket
 FROM events CROSS JOIN span s
), totals AS (
 SELECT bucket,count(*) FILTER(WHERE kind='received') AS received,count(*) FILTER(WHERE kind='closed') AS closed,count(*) FILTER(WHERE kind='cancelled') AS cancelled
 FROM grouped GROUP BY bucket
), bucket_ranges AS (
 SELECT bucket::date AS bucket,
 greatest(bucket::date,s.first_day) AS row_start,
 least((bucket+CASE WHEN s.days<=31 THEN interval '0 days' WHEN s.days<=90 THEN interval '6 days' ELSE interval '1 month - 1 day' END)::date,s.final_day) AS row_end
 FROM span s CROSS JOIN LATERAL generate_series(
  CASE WHEN s.days<=31 THEN s.first_day::timestamp WHEN s.days<=90 THEN date_trunc('week',s.first_day::timestamp) ELSE date_trunc('month',s.first_day::timestamp) END,
  s.final_day::timestamp,
  CASE WHEN s.days<=31 THEN interval '1 day' WHEN s.days<=90 THEN interval '1 week' ELSE interval '1 month' END
 ) bucket
), periods AS (
 SELECT b.*,least((b.row_end+1)::timestamp AT TIME ZONE 'Asia/Irkutsk',p.hi) AS end_at
 FROM bucket_ranges b CROSS JOIN p
), period_values AS (
 SELECT b.bucket,b.row_start,b.row_end,
  coalesce(tot.received,0) AS received,coalesce(tot.closed,0) AS closed,coalesce(tot.cancelled,0) AS cancelled,
  (SELECT count(*) FROM analytics.state_intervals q JOIN analytics.tickets t ON t.id=q.ticket_id
   WHERE q.started_at<b.end_at
    AND (q.ended_at IS NULL OR q.ended_at>=b.end_at)
    AND q.status IN ('new','in_progress','waiting_client')
    [[ AND q.department={{department}} ]] [[ AND q.assignee_id={{employee}} ]] [[ AND t.company_id={{company}} ]]) AS open_count
 FROM periods b LEFT JOIN totals tot ON tot.bucket=b.bucket
), visible AS (
 SELECT * FROM period_values WHERE received>0 OR closed>0 OR cancelled>0 OR open_count>0
)
SELECT CASE WHEN s.unavailable THEN 'Данные ещё не загружены'
 WHEN x.bucket IS NULL THEN 'За выбранный период нет заявок'
 WHEN s.days<=31 AND x.bucket=s.final_day AND s.partial AND s.final_day=(now() AT TIME ZONE 'Asia/Irkutsk')::date THEN 'Сегодня, на момент обновления'
 WHEN s.days<=31 THEN to_char(x.bucket,'DD.MM.YYYY')
 ELSE to_char(x.row_start,'DD.MM.YYYY')||'–'||to_char(x.row_end,'DD.MM.YYYY') END AS "Дата / период",
 x.received AS "Поступило",x.closed AS "Закрыто",x.cancelled AS "Отменено",x.open_count AS "Открытые",
 x.row_start AS "Начало строки",x.row_end AS "Конец строки"
FROM span s LEFT JOIN visible x ON true ORDER BY x.bucket DESC
