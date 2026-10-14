-- psql -X -v ON_ERROR_STOP=1 -f check-privileges.sql against SOURCE database.
\set ON_ERROR_STOP on
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.oid, n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE c.relkind IN ('r','p','v','m','f') AND n.nspname NOT IN ('pg_catalog','information_schema')
               AND n.nspname NOT LIKE 'pg_toast%' LOOP
        IF r.nspname <> 'analytics' AND has_table_privilege('bvk_metabase_reader', r.oid, 'SELECT') THEN
            RAISE EXCEPTION 'Reader can read outside analytics: %.%', r.nspname, r.relname;
        END IF;
        IF has_table_privilege('bvk_metabase_reader', r.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER') THEN
            RAISE EXCEPTION 'Reader has write privilege: %.%', r.nspname, r.relname;
        END IF;
        IF r.nspname <> 'analytics' AND has_table_privilege('bvk_analytics_writer', r.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER') THEN
            RAISE EXCEPTION 'Writer can change source: %.%', r.nspname, r.relname;
        END IF;
        IF r.nspname <> 'analytics' AND NOT (r.nspname='public' AND r.relname IN ('bvk_tickets','bvk_messages','bvk_events','bvk_users','bvk_companies'))
           AND has_table_privilege('bvk_analytics_writer', r.oid, 'SELECT') THEN
            RAISE EXCEPTION 'Writer can read extra source: %.%', r.nspname, r.relname;
        END IF;
    END LOOP;
    FOR r IN SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg_%' AND nspname <> 'information_schema' LOOP
        IF has_schema_privilege('bvk_metabase_reader',r.nspname,'CREATE') OR has_schema_privilege('bvk_analytics_writer',r.nspname,'CREATE') THEN
            RAISE EXCEPTION 'Runtime roles can create in schema %. Revoke PUBLIC CREATE after assessing bot impact.', r.nspname;
        END IF;
    END LOOP;
END $$;
SELECT 'Runtime privilege boundaries passed' AS result;
