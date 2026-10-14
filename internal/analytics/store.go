package analytics

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var ErrBusy = errors.New("another analytics refresh is running")

type Store struct{ Pool *pgxpool.Pool }

// Migrate is an explicit administrator operation, never invoked by Refresh.
func (s Store) Migrate(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Refresh reads and publishes in a single repeatable-read transaction. The lock
// prevents a slow older snapshot from overwriting a newer one. DELETE (not
// TRUNCATE) lets concurrent readers finish against the prior MVCC snapshot.
func (s Store) Refresh(ctx context.Context, opts Options) (result Snapshot, err error) {
	attemptAt := time.Now().UTC()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return result, err
	}
	defer func() {
		_ = tx.Rollback(context.Background())
		if err != nil && !errors.Is(err, ErrBusy) {
			failureCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Do not persist DSNs, SQL arguments or source payloads in the dashboard.
			_, _ = s.Pool.Exec(failureCtx, "UPDATE analytics.refresh_state SET last_attempt_at=$1,last_error='Расчёт не выполнен; см. журнал процесса' WHERE id=1 AND (last_attempt_at IS NULL OR last_attempt_at <= $1)", attemptAt)
		}
	}()
	var locked bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(731903)").Scan(&locked); err != nil {
		return result, err
	}
	if !locked {
		return result, ErrBusy
	}
	source, err := readSource(ctx, tx)
	if err != nil {
		return result, err
	}
	result = Calculate(source, opts)
	if err = publish(ctx, tx, result); err != nil {
		return result, err
	}
	err = tx.Commit(ctx)
	return result, err
}

func readJSON[T any](ctx context.Context, tx pgx.Tx, query string) ([]T, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []T
	for rows.Next() {
		var raw []byte
		var value T
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("decode source row: %w", err)
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func readSource(ctx context.Context, tx pgx.Tx) (src Source, err error) {
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&src.At); err != nil {
		return
	}
	src.Tickets, err = readJSON[domain.Ticket](ctx, tx, "SELECT payload || jsonb_build_object('id',id) FROM public.bvk_tickets ORDER BY id")
	if err != nil {
		return
	}
	src.Events, err = readJSON[domain.Event](ctx, tx, "SELECT payload || jsonb_build_object('ID',id,'TicketID',ticket_id) FROM public.bvk_events ORDER BY id")
	if err != nil {
		return
	}
	// Only metadata is read. Message bodies, attachments and file paths are excluded.
	src.Messages, err = readJSON[domain.Message](ctx, tx, `SELECT jsonb_build_object('ID',id,'TicketID',ticket_id,'AuthorID',payload->'AuthorID','Direction',payload->'Direction','CreatedAt',payload->'CreatedAt','EditedAt',payload->'EditedAt') FROM public.bvk_messages ORDER BY id`)
	if err != nil {
		return
	}
	src.Users, err = readJSON[domain.User](ctx, tx, "SELECT payload || jsonb_build_object('id',id) FROM public.bvk_users ORDER BY id")
	if err != nil {
		return
	}
	rows, e := tx.Query(ctx, "SELECT id,name FROM public.bvk_companies ORDER BY id")
	if e != nil {
		err = e
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c domain.Company
		if err = rows.Scan(&c.ID, &c.Name); err != nil {
			return
		}
		src.Companies = append(src.Companies, c)
	}
	err = rows.Err()
	return
}

func copyRows(ctx context.Context, tx pgx.Tx, table string, columns []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"analytics", table}, columns, pgx.CopyFromRows(rows))
	return err
}

func publish(ctx context.Context, tx pgx.Tx, s Snapshot) error {
	for _, table := range []string{"tickets", "cycles", "events", "state_intervals", "employees", "companies", "departments"} {
		if _, err := tx.Exec(ctx, "DELETE FROM analytics."+table); err != nil {
			return err
		}
	}
	var rows [][]any
	for _, t := range s.Tickets {
		rows = append(rows, []any{t.ID, t.ClientID, t.CompanyID, t.ClientName, t.Description, t.Status, t.Department, t.AssigneeID, t.FirstAssigneeID, t.CreatedAt, t.UpdatedAt, t.FirstResponseAt, t.FirstResponseBy, t.CycleStartedAt, t.LastActivityAt, t.WaitingSince, t.FirstResponseWorkSeconds, t.ResponseWaitingWorkSeconds, t.IdleWorkSeconds, t.WaitingWorkSeconds, t.NoAssignee, t.ResponseOverdue, t.IdleOverdue, t.WaitingOverdue, t.Incomplete, t.Quality})
	}
	if err := copyRows(ctx, tx, "tickets", []string{"id", "client_id", "company_id", "client_name", "description", "status", "department", "assignee_id", "first_assignee_id", "created_at", "updated_at", "first_response_at", "first_response_by", "cycle_started_at", "last_activity_at", "waiting_since", "first_response_work_seconds", "response_waiting_work_seconds", "idle_work_seconds", "waiting_work_seconds", "no_assignee", "response_overdue", "idle_overdue", "waiting_overdue", "incomplete", "quality"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, c := range s.Cycles {
		rows = append(rows, []any{c.TicketID, c.Number, c.StartedAt, c.EndedAt, c.Outcome, c.Department, c.ClosedBy, c.Resolution, c.FirstResponseAt, c.FirstResponseBy, c.FirstResponseDepartment, c.CalendarSeconds, c.WorkSeconds, c.WaitingSeconds, c.WaitingWorkSeconds, c.WorkWithoutWaitingSeconds, c.FirstResponseSeconds, c.FirstResponseWorkSeconds, c.Incomplete})
	}
	if err := copyRows(ctx, tx, "cycles", []string{"ticket_id", "number", "started_at", "ended_at", "outcome", "department", "closed_by", "resolution", "first_response_at", "first_response_by", "first_response_department", "calendar_seconds", "work_seconds", "waiting_seconds", "waiting_work_seconds", "work_without_waiting_seconds", "first_response_seconds", "first_response_work_seconds", "incomplete"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, e := range s.Events {
		rows = append(rows, []any{e.ID, e.TicketID, e.Kind, e.At, e.ActorID, e.Department, e.FromDepartment, e.ToDepartment, e.AssigneeID, e.Status, e.CycleNumber, e.Detail, e.Incomplete})
	}
	if err := copyRows(ctx, tx, "events", []string{"id", "ticket_id", "kind", "occurred_at", "actor_id", "department", "from_department", "to_department", "assignee_id", "status", "cycle_number", "detail", "incomplete"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, i := range s.Intervals {
		rows = append(rows, []any{i.TicketID, i.StartedAt, i.EndedAt, i.Status, i.Department, i.AssigneeID, i.CycleNumber, i.Incomplete})
	}
	if err := copyRows(ctx, tx, "state_intervals", []string{"ticket_id", "started_at", "ended_at", "status", "department", "assignee_id", "cycle_number", "incomplete"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, u := range s.Users {
		if u.Role == "client" {
			continue
		}
		depts := make([]string, len(u.Departments))
		for i, d := range u.Departments {
			depts[i] = string(d)
		}
		rows = append(rows, []any{u.ID, u.Name, u.Role, depts})
	}
	if err := copyRows(ctx, tx, "employees", []string{"id", "name", "role", "departments"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, c := range s.Companies {
		rows = append(rows, []any{c.ID, c.Name})
	}
	if err := copyRows(ctx, tx, "companies", []string{"id", "name"}, rows); err != nil {
		return err
	}
	rows = [][]any{{"it", "IT"}, {"1c", "1С"}, {"installation", "Монтаж"}, {"field", "Выезд"}}
	if err := copyRows(ctx, tx, "departments", []string{"code", "name"}, rows); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE analytics.refresh_state SET snapshot_at=$1,completed_at=clock_timestamp(),calculation_version=$2,incomplete_count=$3,ticket_count=$4,event_count=$5,last_attempt_at=$1,last_error='' WHERE id=1`, s.At, Version, s.IncompleteCount, len(s.Tickets), len(s.Events))
	return err
}
