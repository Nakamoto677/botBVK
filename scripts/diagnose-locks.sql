-- Read-only diagnostics. Run against the bot database with sufficient visibility
-- (pg_read_all_stats or the service owner). No message bodies or SQL text emitted.
SELECT pid, application_name, state, wait_event_type, wait_event,
       clock_timestamp()-xact_start AS transaction_age,
       clock_timestamp()-query_start AS current_operation_age,
       pg_blocking_pids(pid) AS blocking_pids
FROM pg_stat_activity
WHERE datname=current_database() AND pid<>pg_backend_pid()
  AND (state<>'idle' OR cardinality(pg_blocking_pids(pid))>0)
ORDER BY xact_start NULLS LAST;

SELECT l.pid,l.mode,l.granted,l.classid,l.objid,l.objsubid,
       CASE WHEN l.classid=0 AND l.objid=731901 THEN 'schema migration'
            WHEN l.classid=0 AND l.objid=731902 THEN 'single active bot'
            WHEN l.classid=0 AND l.objid=731903 THEN 'inbox / deletion fence'
            WHEN l.classid=0 AND l.objid=731904 THEN 'analytics refresh'
            ELSE 'user / other advisory lock' END AS purpose
FROM pg_locks l
JOIN pg_database d ON d.oid=l.database
WHERE l.locktype='advisory' AND d.datname=current_database()
ORDER BY l.granted,l.pid;

SELECT count(*) FILTER (WHERE NOT finished) AS inbox_pending,
       count(*) FILTER (WHERE NOT finished AND quarantined) AS inbox_quarantined,
       count(*) FILTER (WHERE NOT finished AND NOT quarantined AND available_at>now()) AS inbox_waiting_retry
FROM bvk_updates;

SELECT kind,state,count(*) AS operations,
       min(available_at) AS earliest_available,
       count(*) FILTER (WHERE state='processing' AND lease_until<now()) AS expired_leases
FROM bvk_outbox WHERE state IN ('pending','processing','failed')
GROUP BY kind,state ORDER BY kind,state;
