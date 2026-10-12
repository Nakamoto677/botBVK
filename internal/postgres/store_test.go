package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSchemaSafety(t *testing.T) {
	for _, required := range []string{"bvk_active_client", "bvk_message_source", "bvk_topic", "REFERENCES bvk_users", "REFERENCES bvk_companies"} {
		if !strings.Contains(schema, required) {
			t.Errorf("schema lacks %s", required)
		}
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + "_" + time.Now().Format("150405000000")
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		base.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool}
	t.Cleanup(func() { s.Close(); base.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); base.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
	if err = s.Bootstrap(ctx, 99, []string{"Example"}); err != nil {
		t.Fatal(err)
	}
	return s
}
func seed(t *testing.T, s *Store) domain.Ticket {
	t.Helper()
	ctx := context.Background()
	companies, err := s.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := domain.Ticket{ClientID: 1, CompanyID: companies[0].ID, Department: domain.IT, Status: domain.New, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	err = s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveUser(ctx, domain.User{ID: 1, CompanyID: v.CompanyID, Approved: true, Role: "client"}); err != nil {
			return err
		}
		return tx.SaveTicket(ctx, &v)
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestTicketConcurrencyAndArchive(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	duplicate := v
	duplicate.ID = 0
	err := s.Transact(ctx, func(tx domain.Tx) error { return tx.SaveTicket(ctx, &duplicate) })
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate active ticket: %v", err)
	}
	stale := v
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(actor int64) {
			defer wg.Done()
			results <- s.Transact(ctx, func(tx domain.Tx) error {
				ticket, err := tx.GetTicket(ctx, v.ID)
				if err != nil {
					return err
				}
				if ticket.AssigneeID != 0 {
					return domain.ErrConflict
				}
				ticket.AssigneeID = actor
				ticket.Status = domain.InProgress
				return tx.SaveTicket(ctx, &ticket)
			})
		}(int64(i + 10))
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("assignment winners: %d", success)
	}
	err = s.Transact(ctx, func(tx domain.Tx) error { return tx.SaveTicket(ctx, &stale) })
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	msg := domain.Message{TicketID: v.ID, AuthorID: 1, Direction: "inbound", TelegramChatID: 1, TelegramMessageID: 123, Text: "Issue"}
	err = s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.AddMessage(ctx, &msg); err != nil {
			return err
		}
		return tx.AddEvent(ctx, domain.Event{TicketID: v.ID, Kind: "message", Data: json.RawMessage(`{}`)})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Transact(ctx, func(tx domain.Tx) error { duplicate := msg; return tx.AddMessage(ctx, &duplicate) })
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("message duplicate: %v", err)
	}
	if err = s.SaveArchive(ctx, msg.ID, "archive/a", "sha"); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(ctx, v.ID)
	if err != nil || len(messages) != 1 || messages[0].ArchivePath != "archive/a" || messages[0].SHA256 != "sha" {
		t.Fatalf("archive: %#v %v", messages, err)
	}
	events, err := s.Events(ctx, v.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %#v %v", events, err)
	}
}
func TestInboxOutboxRollbackAndRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fresh, err := s.SaveUpdate(ctx, 12, json.RawMessage(`{"update_id":12}`))
	if err != nil || !fresh {
		t.Fatalf("save: %v", err)
	}
	fresh, err = s.SaveUpdate(ctx, 12, json.RawMessage(`{}`))
	if err != nil || fresh {
		t.Fatalf("dedup: %v", err)
	}
	id, err := s.LastUpdateID(ctx)
	if err != nil || id != 12 {
		t.Fatalf("cursor: %d %v", id, err)
	}
	updates, err := s.PendingUpdates(ctx, 10)
	if err != nil || len(updates) != 1 {
		t.Fatal(err)
	}
	if err = s.FinishUpdate(ctx, 12); err != nil {
		t.Fatal(err)
	}
	updates, err = s.PendingUpdates(ctx, 10)
	if err != nil || len(updates) != 0 {
		t.Fatal(err)
	}
	err = s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.Enqueue(ctx, domain.Operation{Kind: "rolledback"}); err != nil {
			return err
		}
		return domain.ErrInvalid
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = s.NextOperation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rollback: %v", err)
	}
	if err = s.Transact(ctx, func(tx domain.Tx) error {
		return tx.Enqueue(ctx, domain.Operation{Kind: "send", Payload: json.RawMessage(`{"text":"hello"}`)})
	}); err != nil {
		t.Fatal(err)
	}
	op, err := s.NextOperation(ctx)
	if err != nil || op.Attempts != 1 {
		t.Fatalf("lease: %#v %v", op, err)
	}
	if _, err = s.NextOperation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("leased twice: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE bvk_outbox SET lease_until=now()-interval '1 second' WHERE id=$1", op.ID); err != nil {
		t.Fatal(err)
	}
	op, err = s.NextOperation(ctx)
	if err != nil || op.Attempts != 2 {
		t.Fatalf("reclaim: %#v %v", op, err)
	}
	if err = s.RetryOperation(ctx, op.ID, "rate limit", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextOperation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delayed retry: %v", err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE bvk_outbox SET available_at=now() WHERE id=$1", op.ID); err != nil {
		t.Fatal(err)
	}
	op, err = s.NextOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishOperation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextOperation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("done operation replay: %v", err)
	}
}
func TestAccessAndBootstrap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	companies, _ := s.Companies(ctx)
	var request domain.AccessRequest
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveUser(ctx, domain.User{ID: 44, Name: "Client", Role: "client"}); err != nil {
			return err
		}
		return tx.RequestAccess(ctx, 44, companies[0].ID)
	}); err != nil {
		t.Fatal(err)
	}
	requests, err := s.AccessRequests(ctx)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v %v", requests, err)
	}
	if err = s.Transact(ctx, func(tx domain.Tx) error {
		var err error
		request, err = tx.ResolveAccess(ctx, requests[0].ID, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	u, err := s.User(ctx, 44)
	if err != nil || !u.Approved || u.CompanyID != request.CompanyID {
		t.Fatalf("approval: %#v %v", u, err)
	}
	if err = s.Bootstrap(ctx, 44, []string{"Example"}); err != nil {
		t.Fatal(err)
	}
	u, _ = s.User(ctx, 44)
	if u.Role != "client" {
		t.Fatal("bootstrap overwrote user")
	}
}
