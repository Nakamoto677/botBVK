package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestConversationOrderIncludesUIAndTicketNotifications(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := t.Context()
	entries := []domain.Operation{
		{Kind: "ui_screen", Payload: json.RawMessage(`{"user_id":1,"chat_id":1,"text":"form"}`)},
		{Kind: "notify_client", TicketID: ticket.ID, Payload: json.RawMessage(`{"text":"accepted"}`)},
		{Kind: "ui_screen", Payload: json.RawMessage(`{"user_id":2,"chat_id":2,"text":"other"}`)},
	}
	for _, op := range entries {
		if err := s.Transact(ctx, func(tx domain.Tx) error { return tx.Enqueue(ctx, op) }); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.NextOperation(ctx)
	if err != nil || first.Kind != "ui_screen" {
		t.Fatal(first, err)
	}
	other, err := s.NextOperation(ctx)
	if err != nil || other.TicketID != 0 || other.ID <= first.ID {
		t.Fatal(other, err)
	}
	if _, err = s.NextOperation(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("accepted notice overtook form", err)
	}
	if err = s.FinishOperation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	notice, err := s.NextOperation(ctx)
	if err != nil || notice.Kind != "notify_client" {
		t.Fatal(notice, err)
	}
}

func TestBurstCoalescesPendingCardRefreshButNotProcessing(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := t.Context()
	enqueue := func() {
		t.Helper()
		if err := s.Transact(ctx, func(tx domain.Tx) error {
			return tx.Enqueue(ctx, domain.Operation{Kind: "refresh_card", TicketID: ticket.ID})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		enqueue()
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	first, err := s.NextOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	enqueue()
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox").Scan(&count); err != nil || count != 2 {
		t.Fatal("processing refresh swallowed new state", count, err)
	}
	if err = s.FinishOperation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextOperation(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSystemAndUserLocksHaveSeparateKeys(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	held, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err = held.Exec(ctx, "SELECT pg_advisory_xact_lock(731903)"); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err = s.Transact(deadline, func(tx domain.Tx) error { return tx.SaveUser(deadline, domain.User{ID: 731903, Role: "client"}) }); err != nil {
		t.Fatal("system lock blocked coincident Telegram ID", err)
	}
}

func TestDeadlockAndSerializationErrorsRemainRetryable(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		source := &pgconn.PgError{Code: code}
		got := mapError(source)
		var actual *pgconn.PgError
		if errors.Is(got, domain.ErrConflict) || !errors.As(got, &actual) {
			t.Fatalf("%s mapped to completed business error: %v", code, got)
		}
	}
}

func TestBurstMigrationBackfillsExistingConversationWithoutDeletingData(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx, "DELETE FROM bvk_migrations WHERE version='008_burst_delivery'"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []struct {
		kind    string
		ticket  int64
		payload string
	}{{"notify_client", ticket.ID, `{"text":"saved"}`}, {"ui_screen", 0, `{"user_id":1,"chat_id":1,"ui_key":"saved"}`}} {
		if _, err := s.pool.Exec(ctx, "INSERT INTO bvk_outbox(kind,ticket_id,payload) VALUES($1,$2,$3)", op.kind, op.ticket, op.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox WHERE conversation_id=1").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	saved, err := s.Ticket(ctx, ticket.ID)
	if err != nil || saved.Version != ticket.Version {
		t.Fatal("ticket changed", saved, err)
	}
}

func TestPendingBurstSelectionCostAndOrder(t *testing.T) {
	s := testStore(t)
	seed(t, s)
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx, `INSERT INTO bvk_updates(id,payload,finished) SELECT n,'{}',true FROM generate_series(1,10000) n`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		raw := json.RawMessage(fmt.Sprintf(`{"message":{"from":{"id":%d},"chat":{"id":%d,"type":"private"},"text":"burst"}}`, i%10+1, i%10+1))
		if _, err := s.SaveUpdate(ctx, int64(10001+i), raw); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	for i := 0; i < 100; i++ {
		pending, err := s.PendingUpdates(ctx, 1)
		if err != nil || len(pending) != 1 || pending[0].ID != int64(10001+i) {
			t.Fatalf("%v %v", pending, err)
		}
		if err = s.FinishUpdate(ctx, pending[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("100 ordered updates with 10000 completed rows: %s", time.Since(started))
}

func TestExpiredLeaseDoesNotReclaimRunningJob(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	for _, user := range []int64{1, 2} {
		raw := json.RawMessage(fmt.Sprintf(`{"user_id":%d,"chat_id":%d,"text":"work"}`, user, user))
		if err := s.Transact(ctx, func(tx domain.Tx) error { return tx.Enqueue(ctx, domain.Operation{Kind: "ui_screen", Payload: raw}) }); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.NextOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE bvk_outbox SET lease_until=now()-interval '1 minute' WHERE id=$1", first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.NextOperationExcluding(ctx, []int64{first.ID})
	if err != nil || second.ID == first.ID {
		t.Fatalf("live job reclaimed: %+v %v", second, err)
	}
	recovered, err := s.NextOperation(ctx)
	if err != nil || recovered.ID != first.ID {
		t.Fatalf("restart cannot recover expired job: %+v %v", recovered, err)
	}
}
