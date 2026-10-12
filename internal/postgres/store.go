// Package postgres implements durable transactional storage for the support bot.
package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

//go:embed queue_on_claim.sql
var queueOnClaimMigration string

type Store struct{ pool *pgxpool.Pool }
type transaction struct{ tx pgx.Tx }

var _ domain.Store = (*Store)(nil)
var _ domain.Tx = (*transaction)(nil)

func New(ctx context.Context, dsn string) (*Store, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{pool: p}, nil
}
func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Migrate(ctx context.Context) error {
	return s.Transact(ctx, func(t domain.Tx) error {
		tx := t.(*transaction).tx
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(731901)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, schema); err != nil {
			return err
		}
		marker, err := tx.Exec(ctx, "INSERT INTO bvk_migrations(version) VALUES('002_queue_on_claim') ON CONFLICT(version) DO NOTHING")
		if err != nil {
			return err
		}
		if marker.RowsAffected() == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, queueOnClaimMigration)
		return err
	})
}
func (s *Store) Bootstrap(ctx context.Context, adminID int64, companies []string) error {
	return s.Transact(ctx, func(t domain.Tx) error {
		tx := t.(*transaction).tx
		for _, name := range companies {
			if _, err := tx.Exec(ctx, "INSERT INTO bvk_companies(name) VALUES($1) ON CONFLICT(name) DO NOTHING", name); err != nil {
				return err
			}
		}
		if adminID == 0 {
			return nil
		}
		u, err := t.GetUser(ctx, adminID)
		if errors.Is(err, domain.ErrNotFound) {
			u = domain.User{ID: adminID, Name: "Administrator"}
		} else if err != nil {
			return err
		} else {
			return nil
		}
		u.Role = "admin"
		u.Approved = true
		return t.SaveUser(ctx, u)
	})
}
func mapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505", "40001", "40P01":
			return fmt.Errorf("%w: %s", domain.ErrConflict, pg.Message)
		case "23503", "23514":
			return fmt.Errorf("%w: %s", domain.ErrInvalid, pg.Message)
		}
	}
	return err
}
func (s *Store) Transact(ctx context.Context, fn func(domain.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = fn(&transaction{tx}); err != nil {
		return mapError(err)
	}
	return mapError(tx.Commit(ctx))
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readJSON[T any](ctx context.Context, q querier, sql string, args ...any) (T, error) {
	var result T
	var raw []byte
	err := q.QueryRow(ctx, sql, args...).Scan(&raw)
	if err != nil {
		return result, mapError(err)
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}
func listJSON[T any](ctx context.Context, q querier, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		var raw []byte
		var v T
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (t *transaction) GetUser(ctx context.Context, id int64) (domain.User, error) {
	if _, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", id); err != nil {
		return domain.User{}, err
	}
	return readJSON[domain.User](ctx, t.tx, "SELECT payload FROM bvk_users WHERE id=$1 FOR UPDATE", id)
}
func (t *transaction) SaveUser(ctx context.Context, u domain.User) error {
	if _, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", u.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, "INSERT INTO bvk_users(id,payload) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET payload=EXCLUDED.payload", u.ID, raw)
	return mapError(err)
}
func (t *transaction) GetTicket(ctx context.Context, id int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, t.tx, "SELECT payload FROM bvk_tickets WHERE id=$1 FOR UPDATE", id)
}
func (t *transaction) ActiveTicket(ctx context.Context, id int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, t.tx, "SELECT payload FROM bvk_tickets WHERE client_id=$1 AND status IN ('new','in_progress','waiting_client') FOR UPDATE", id)
}
func (t *transaction) SaveTicket(ctx context.Context, v *domain.Ticket) error {
	if v == nil {
		return domain.ErrInvalid
	}
	copy := *v
	if copy.ID == 0 {
		if _, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", copy.ClientID); err != nil {
			return err
		}
		if err := t.tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('bvk_tickets','id'))").Scan(&copy.ID); err != nil {
			return err
		}
		copy.Version = 1
		raw, err := json.Marshal(copy)
		if err != nil {
			return err
		}
		_, err = t.tx.Exec(ctx, "INSERT INTO bvk_tickets(id,client_id,company_id,status,department,version,topic_chat_id,topic_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", copy.ID, copy.ClientID, copy.CompanyID, copy.Status, copy.Department, copy.Version, copy.TopicChatID, copy.TopicID, raw)
		if err != nil {
			return mapError(err)
		}
	} else {
		oldVersion := copy.Version
		copy.Version++
		raw, err := json.Marshal(copy)
		if err != nil {
			return err
		}
		tag, err := t.tx.Exec(ctx, "UPDATE bvk_tickets SET client_id=$2,company_id=$3,status=$4,department=$5,version=$6,topic_chat_id=$7,topic_id=$8,payload=$9 WHERE id=$1 AND version=$10", copy.ID, copy.ClientID, copy.CompanyID, copy.Status, copy.Department, copy.Version, copy.TopicChatID, copy.TopicID, raw, oldVersion)
		if err != nil {
			return mapError(err)
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	*v = copy
	return nil
}
func (t *transaction) AddMessage(ctx context.Context, v *domain.Message) error {
	if v == nil {
		return domain.ErrInvalid
	}
	copy := *v
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now().UTC()
	}
	if err := t.tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('bvk_messages','id'))").Scan(&copy.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, "INSERT INTO bvk_messages(id,ticket_id,telegram_chat_id,telegram_message_id,direction,payload) VALUES($1,$2,$3,$4,$5,$6)", copy.ID, copy.TicketID, copy.TelegramChatID, copy.TelegramMessageID, copy.Direction, raw)
	if err == nil {
		*v = copy
	}
	return mapError(err)
}

// EditMessage changes only text and edit history; source and archive metadata
// remain attached to the original message. Revisions contain prior versions,
// oldest first. The original revision timestamp is its creation time.
func (t *transaction) EditMessage(ctx context.Context, v *domain.Message) error {
	if v == nil || v.TelegramMessageID == 0 || v.EditedAt == nil {
		return domain.ErrInvalid
	}
	matches, err := listJSON[domain.Message](ctx, t.tx, "SELECT payload FROM bvk_messages WHERE telegram_chat_id=$1 AND telegram_message_id=$2 AND ticket_id=$3 AND ($4='' OR direction=$4) ORDER BY id LIMIT 2 FOR UPDATE", v.TelegramChatID, v.TelegramMessageID, v.TicketID, v.Direction)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return domain.ErrNotFound
	}
	if len(matches) != 1 {
		return domain.ErrConflict
	}
	stored := matches[0]
	if stored.AuthorID != v.AuthorID {
		return domain.ErrForbidden
	}
	if stored.Text == v.Text || (stored.EditedAt != nil && v.EditedAt.Before(*stored.EditedAt)) {
		return domain.ErrConflict
	}
	previousTime := stored.CreatedAt
	if stored.EditedAt != nil {
		previousTime = *stored.EditedAt
	}
	stored.Revisions = append(stored.Revisions, domain.MessageRevision{Text: stored.Text, EditedAt: previousTime})
	stored.Text = v.Text
	edited := v.EditedAt.UTC()
	stored.EditedAt = &edited
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, "UPDATE bvk_messages SET payload=$2 WHERE id=$1", stored.ID, raw)
	if err == nil {
		*v = stored
	}
	return err
}

func (t *transaction) AddEvent(ctx context.Context, v domain.Event) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	if err := t.tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('bvk_events','id'))").Scan(&v.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, "INSERT INTO bvk_events(id,ticket_id,payload) VALUES($1,$2,$3)", v.ID, v.TicketID, raw)
	return mapError(err)
}
func (t *transaction) Enqueue(ctx context.Context, v domain.Operation) error {
	// Scheduling and leasing must share the database clock. Even small host/DB
	// clock skew can otherwise make an immediate operation appear in the future.
	var available *time.Time
	if !v.AvailableAt.IsZero() {
		available = &v.AvailableAt
	}
	if len(v.Payload) == 0 {
		v.Payload = json.RawMessage(`{}`)
	}
	_, err := t.tx.Exec(ctx, "INSERT INTO bvk_outbox(kind,ticket_id,payload,available_at) VALUES($1,$2,$3,COALESCE($4::timestamptz,now()))", v.Kind, v.TicketID, v.Payload, available)
	return err
}
func (t *transaction) RequestAccess(ctx context.Context, userID, companyID int64) error {
	_, err := t.tx.Exec(ctx, "INSERT INTO bvk_access(user_id,company_id) VALUES($1,$2) ON CONFLICT(user_id) WHERE status='pending' DO UPDATE SET company_id=EXCLUDED.company_id", userID, companyID)
	return mapError(err)
}
func (t *transaction) ResolveAccess(ctx context.Context, id int64, approve bool) (domain.AccessRequest, error) {
	var r domain.AccessRequest
	err := t.tx.QueryRow(ctx, "SELECT id,user_id,company_id,status,created_at FROM bvk_access WHERE id=$1", id).Scan(&r.ID, &r.UserID, &r.CompanyID, &r.Status, &r.CreatedAt)
	if err != nil {
		return r, mapError(err)
	}
	u, err := t.GetUser(ctx, r.UserID)
	if err != nil {
		return r, err
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	err = t.tx.QueryRow(ctx, "UPDATE bvk_access SET status=$2 WHERE id=$1 AND status='pending' RETURNING id,user_id,company_id,status,created_at", id, status).Scan(&r.ID, &r.UserID, &r.CompanyID, &r.Status, &r.CreatedAt)
	if err != nil {
		return r, mapError(err)
	}
	if approve {
		u.Approved = true
		u.CompanyID = r.CompanyID
		if err = t.SaveUser(ctx, u); err != nil {
			return r, err
		}
	}
	return r, nil
}

func (s *Store) User(ctx context.Context, id int64) (domain.User, error) {
	return readJSON[domain.User](ctx, s.pool, "SELECT payload FROM bvk_users WHERE id=$1", id)
}
func (s *Store) Ticket(ctx context.Context, id int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, s.pool, "SELECT payload FROM bvk_tickets WHERE id=$1", id)
}
func (s *Store) ActiveTicket(ctx context.Context, id int64) (domain.Ticket, error) {
	return readJSON[domain.Ticket](ctx, s.pool, "SELECT payload FROM bvk_tickets WHERE client_id=$1 AND status IN ('new','in_progress','waiting_client')", id)
}
func (s *Store) TopicTicket(ctx context.Context, chatID, topicID int64) (domain.Ticket, error) {
	if topicID == 0 {
		return domain.Ticket{}, domain.ErrNotFound
	}
	return readJSON[domain.Ticket](ctx, s.pool, "SELECT payload FROM bvk_tickets WHERE topic_chat_id=$1 AND topic_id=$2", chatID, topicID)
}
func (s *Store) Tickets(ctx context.Context) ([]domain.Ticket, error) {
	return listJSON[domain.Ticket](ctx, s.pool, "SELECT payload FROM bvk_tickets ORDER BY id")
}
func (s *Store) Messages(ctx context.Context, id int64) ([]domain.Message, error) {
	return listJSON[domain.Message](ctx, s.pool, "SELECT payload FROM bvk_messages WHERE ticket_id=$1 ORDER BY id", id)
}
func (s *Store) Events(ctx context.Context, id int64) ([]domain.Event, error) {
	return listJSON[domain.Event](ctx, s.pool, "SELECT payload FROM bvk_events WHERE ticket_id=$1 ORDER BY id", id)
}
func (s *Store) Companies(ctx context.Context) ([]domain.Company, error) {
	rows, err := s.pool.Query(ctx, "SELECT id,name FROM bvk_companies ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Company{}
	for rows.Next() {
		var v domain.Company
		if err = rows.Scan(&v.ID, &v.Name); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) AccessRequests(ctx context.Context) ([]domain.AccessRequest, error) {
	rows, err := s.pool.Query(ctx, "SELECT id,user_id,company_id,status,created_at FROM bvk_access WHERE status='pending' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AccessRequest{}
	for rows.Next() {
		var v domain.AccessRequest
		if err = rows.Scan(&v.ID, &v.UserID, &v.CompanyID, &v.Status, &v.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) SaveUpdate(ctx context.Context, id int64, payload json.RawMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx, "INSERT INTO bvk_updates(id,payload) VALUES($1,$2) ON CONFLICT(id) DO NOTHING", id, payload)
	return tag.RowsAffected() == 1, err
}
func (s *Store) PendingUpdates(ctx context.Context, limit int) ([]domain.StoredUpdate, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, "SELECT id,payload FROM bvk_updates WHERE NOT finished ORDER BY id LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.StoredUpdate{}
	for rows.Next() {
		var v domain.StoredUpdate
		if err = rows.Scan(&v.ID, &v.Payload); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) FinishUpdate(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE bvk_updates SET finished=true WHERE id=$1", id)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return err
}
func (s *Store) LastUpdateID(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT COALESCE(max(id),0) FROM bvk_updates").Scan(&id)
	return id, err
}
func (s *Store) NextOperation(ctx context.Context) (domain.Operation, error) {
	var v domain.Operation
	err := s.pool.QueryRow(ctx, `WITH picked AS (SELECT candidate.id FROM bvk_outbox candidate WHERE ((candidate.state='pending' AND candidate.available_at<=now()) OR (candidate.state='processing' AND candidate.lease_until<=now())) AND NOT EXISTS (SELECT 1 FROM bvk_outbox older WHERE candidate.ticket_id<>0 AND older.ticket_id=candidate.ticket_id AND older.id<candidate.id AND older.state IN ('pending','processing')) ORDER BY candidate.available_at,candidate.id FOR UPDATE OF candidate SKIP LOCKED LIMIT 1) UPDATE bvk_outbox o SET state='processing',attempts=o.attempts+1,lease_until=now()+interval '5 minutes' FROM picked WHERE o.id=picked.id RETURNING o.id,o.kind,o.ticket_id,o.payload,o.attempts,o.available_at,o.last_error,o.state`).Scan(&v.ID, &v.Kind, &v.TicketID, &v.Payload, &v.Attempts, &v.AvailableAt, &v.LastError, &v.State)
	return v, mapError(err)
}
func (s *Store) FinishOperation(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE bvk_outbox SET state='done',lease_until=NULL WHERE id=$1 AND state='processing'", id)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrConflict
	}
	return err
}
func (s *Store) RetryOperation(ctx context.Context, id int64, lastError string, delay time.Duration, permanent bool) error {
	state := "pending"
	if permanent {
		state = "failed"
	}
	if delay < 0 {
		delay = 0
	}
	tag, err := s.pool.Exec(ctx, "UPDATE bvk_outbox SET state=$2,last_error=$3,available_at=now()+($4::bigint * interval '1 microsecond'),lease_until=NULL WHERE id=$1 AND state='processing'", id, state, lastError, delay.Microseconds())
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrConflict
	}
	return err
}
func (s *Store) SaveArchive(ctx context.Context, id int64, path, sha string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE bvk_messages SET payload=jsonb_set(jsonb_set(payload,'{ArchivePath}',to_jsonb($2::text)),'{SHA256}',to_jsonb($3::text)) WHERE id=$1`, id, path, sha)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return err
}

// QueueHealth reports unfinished and failed operations, plus the earliest due
// time among unfinished operations. Future scheduled work is not overdue.
func (s *Store) QueueHealth(ctx context.Context) (pending int, failed int, oldest time.Time, err error) {
	var earliest *time.Time
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('pending','processing')), count(*) FILTER (WHERE state='failed'), min(available_at) FILTER (WHERE state IN ('pending','processing') AND available_at<=now()) FROM bvk_outbox`).Scan(&pending, &failed, &earliest)
	if earliest != nil {
		oldest = *earliest
	}
	return
}

// SaveDraft persists the client's in-progress ticket form across restarts.
// User approval and authorization are checked by the Telegram adapter.
func (s *Store) SaveDraft(ctx context.Context, userID int64, department domain.Department, text string) error {
	if userID == 0 || !department.Valid() {
		return domain.ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bvk_drafts(user_id,department,body) VALUES($1,$2,$3) ON CONFLICT(user_id) DO UPDATE SET department=EXCLUDED.department,body=EXCLUDED.body,updated_at=now()`, userID, department, text)
	return mapError(err)
}
func (s *Store) Draft(ctx context.Context, userID int64) (domain.Department, string, error) {
	var department domain.Department
	var text string
	err := s.pool.QueryRow(ctx, "SELECT department,body FROM bvk_drafts WHERE user_id=$1", userID).Scan(&department, &text)
	return department, text, mapError(err)
}

// DeleteDraft is idempotent so a repeated confirmation can safely clean up.
func (s *Store) DeleteDraft(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM bvk_drafts WHERE user_id=$1", userID)
	return err
}

// MessageTicket resolves an archived Telegram source even after its ticket has
// closed or the client has opened a different ticket. Ambiguous sources fail
// rather than attach edits to the wrong conversation.
func (s *Store) MessageTicket(ctx context.Context, telegramChatID, telegramMessageID int64) (domain.Ticket, error) {
	if telegramMessageID == 0 {
		return domain.Ticket{}, domain.ErrInvalid
	}
	matches, err := listJSON[domain.Ticket](ctx, s.pool, `SELECT t.payload FROM bvk_tickets t JOIN (SELECT DISTINCT ticket_id FROM bvk_messages WHERE telegram_chat_id=$1 AND telegram_message_id=$2) source ON source.ticket_id=t.id ORDER BY t.id LIMIT 2`, telegramChatID, telegramMessageID)
	if err != nil {
		return domain.Ticket{}, err
	}
	if len(matches) == 0 {
		return domain.Ticket{}, domain.ErrNotFound
	}
	if len(matches) > 1 {
		return domain.Ticket{}, domain.ErrConflict
	}
	return matches[0], nil
}

// Operation inspects a durable delivery operation for authorized diagnostics.
func (s *Store) Operation(ctx context.Context, id int64) (domain.Operation, error) {
	var v domain.Operation
	err := s.pool.QueryRow(ctx, "SELECT id,kind,ticket_id,payload,attempts,available_at,last_error,state FROM bvk_outbox WHERE id=$1", id).Scan(&v.ID, &v.Kind, &v.TicketID, &v.Payload, &v.Attempts, &v.AvailableAt, &v.LastError, &v.State)
	return v, mapError(err)
}

// RetryFailedOperation reuses the original queue position; authorization and
// ambiguous Telegram side-effect checks belong to the administrative adapter.
func (s *Store) RetryFailedOperation(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE bvk_outbox SET state='pending',attempts=0,last_error='',available_at=now(),lease_until=NULL WHERE id=$1 AND state='failed'", id)
	if err == nil && tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return err
}
