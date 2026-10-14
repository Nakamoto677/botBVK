-- psql -X -v ON_ERROR_STOP=1 -v source_db=bvk -f provision.sql
-- Run as PostgreSQL administrator AFTER bvk-analytics -migrate as DB owner.
-- Source tables are in public. Role names must be unused; fail rather than
-- silently reusing a role that may have elevated rights or memberships.
\set ON_ERROR_STOP on
BEGIN;
CREATE ROLE bvk_analytics_writer LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;
CREATE ROLE bvk_metabase_reader LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;
CREATE ROLE bvk_metabase_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;
GRANT CONNECT ON DATABASE :"source_db" TO bvk_analytics_writer, bvk_metabase_reader;
GRANT USAGE ON SCHEMA public TO bvk_analytics_writer;
GRANT SELECT ON public.bvk_tickets, public.bvk_messages, public.bvk_events,
    public.bvk_users, public.bvk_companies TO bvk_analytics_writer;
REVOKE ALL ON SCHEMA analytics FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA analytics FROM PUBLIC;
GRANT USAGE ON SCHEMA analytics TO bvk_analytics_writer, bvk_metabase_reader;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA analytics TO bvk_analytics_writer;
GRANT SELECT ON ALL TABLES IN SCHEMA analytics TO bvk_metabase_reader;
-- Defaults apply to the migration owner executing this file. If a different
-- owner runs future migrations, set the same defaults FOR ROLE that_owner.
ALTER DEFAULT PRIVILEGES IN SCHEMA analytics GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO bvk_analytics_writer;
ALTER DEFAULT PRIVILEGES IN SCHEMA analytics GRANT SELECT ON TABLES TO bvk_metabase_reader;
COMMIT;
-- CREATE DATABASE cannot run inside a transaction.
CREATE DATABASE bvk_metabase OWNER bvk_metabase_app;
REVOKE ALL ON DATABASE bvk_metabase FROM PUBLIC;
GRANT CONNECT ON DATABASE bvk_metabase TO bvk_metabase_app;
\connect bvk_metabase
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO bvk_metabase_app;
-- Set passwords interactively: \password bvk_analytics_writer etc.
-- Source PUBLIC grants must be audited separately: PostgreSQL has no DENY.
