CREATE SCHEMA IF NOT EXISTS analytics;
CREATE TABLE IF NOT EXISTS analytics.tickets (
 id bigint PRIMARY KEY, client_id bigint NOT NULL, company_id bigint NOT NULL,
 client_name text NOT NULL, description text NOT NULL, status text NOT NULL, department text NOT NULL,
 assignee_id bigint, first_assignee_id bigint, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 first_response_at timestamptz, first_response_by bigint, cycle_started_at timestamptz,
 last_activity_at timestamptz, waiting_since timestamptz,
 first_response_work_seconds double precision, response_waiting_work_seconds double precision, idle_work_seconds double precision, waiting_work_seconds double precision,
 no_assignee boolean NOT NULL, response_overdue boolean NOT NULL, idle_overdue boolean NOT NULL, waiting_overdue boolean NOT NULL,
 incomplete boolean NOT NULL, quality text NOT NULL
);
CREATE TABLE IF NOT EXISTS analytics.cycles (
 ticket_id bigint NOT NULL, number integer NOT NULL, started_at timestamptz NOT NULL, ended_at timestamptz,
 outcome text NOT NULL, department text, closed_by bigint, resolution text NOT NULL,
 first_response_at timestamptz, first_response_by bigint, first_response_department text,
 calendar_seconds double precision, work_seconds double precision, waiting_seconds double precision, waiting_work_seconds double precision,
 work_without_waiting_seconds double precision, first_response_seconds double precision, first_response_work_seconds double precision,
 incomplete boolean NOT NULL, PRIMARY KEY(ticket_id,number)
);
CREATE TABLE IF NOT EXISTS analytics.events (
 id bigint PRIMARY KEY, ticket_id bigint NOT NULL, kind text NOT NULL, occurred_at timestamptz NOT NULL,
 actor_id bigint, department text, from_department text, to_department text, assignee_id bigint,
 status text, cycle_number integer NOT NULL, detail text NOT NULL, incomplete boolean NOT NULL
);
CREATE TABLE IF NOT EXISTS analytics.state_intervals (
 ticket_id bigint NOT NULL, started_at timestamptz NOT NULL, ended_at timestamptz,
 status text NOT NULL, department text, assignee_id bigint, cycle_number integer NOT NULL, incomplete boolean NOT NULL
);
ALTER TABLE analytics.tickets ADD COLUMN IF NOT EXISTS response_waiting_work_seconds double precision;
ALTER TABLE analytics.cycles ADD COLUMN IF NOT EXISTS first_response_department text;
CREATE INDEX IF NOT EXISTS analytics_events_period ON analytics.events(kind,occurred_at,ticket_id);
CREATE INDEX IF NOT EXISTS analytics_intervals_time ON analytics.state_intervals(started_at,ended_at);
-- Index audit 2026-10-22: no report filters cycles by ended_at; every query joins
-- cycles by (ticket_id,number) or reads one ticket's cycles. The index only added
-- work to the minute-by-minute snapshot publish, so old databases drop it here.
DROP INDEX IF EXISTS analytics.analytics_cycles_end;
CREATE TABLE IF NOT EXISTS analytics.employees (id bigint PRIMARY KEY, name text NOT NULL, role text NOT NULL, departments text[] NOT NULL);
CREATE TABLE IF NOT EXISTS analytics.companies (id bigint PRIMARY KEY, name text NOT NULL);
CREATE TABLE IF NOT EXISTS analytics.departments (code text PRIMARY KEY, name text NOT NULL);
CREATE TABLE IF NOT EXISTS analytics.refresh_state (
 id integer PRIMARY KEY CHECK(id=1), snapshot_at timestamptz, completed_at timestamptz,
 calculation_version text NOT NULL, incomplete_count integer NOT NULL DEFAULT 0,
 ticket_count integer NOT NULL DEFAULT 0, event_count integer NOT NULL DEFAULT 0,
 last_attempt_at timestamptz, last_error text NOT NULL DEFAULT ''
);
INSERT INTO analytics.refresh_state(id,calculation_version) VALUES(1,'1') ON CONFLICT DO NOTHING;
