package postgres

import (
	"context"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

func failedKind(t *testing.T, s *Store, ticketID int64, kind string, age time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		return tx.Enqueue(ctx, domain.Operation{Kind: kind, TicketID: ticketID})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT max(id) FROM bvk_outbox WHERE ticket_id=$1`, ticketID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// A failed operation is what the worker leaves behind: terminal, with a reason.
	if _, err := s.pool.Exec(ctx, `UPDATE bvk_outbox SET state='failed', last_error='telegram 400: bad request', available_at=now()-$2::interval WHERE id=$1`, id, age.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func state(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(context.Background(), `SELECT state FROM bvk_outbox WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A failure on a finished ticket has nothing left to deliver, so it is retired and
// stops pinning the readiness probe to "unavailable".
func TestFailedOperationOnFinishedTicketIsRetired(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := context.Background()
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		ticket.Status = domain.Cancelled
		return tx.SaveTicket(ctx, &ticket)
	}); err != nil {
		t.Fatal(err)
	}
	id := failedKind(t, s, ticket.ID, "refresh_card", time.Minute)

	retired, err := s.RetireFinishedFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired %d operations, want 1", retired)
	}
	if got := state(t, s, id); got != "done" {
		t.Fatalf("failed operation left in state %q", got)
	}
	// The reason must survive for the audit trail.
	var reason string
	if err := s.pool.QueryRow(ctx, `SELECT last_error FROM bvk_outbox WHERE id=$1`, id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Fatal("retirement wiped the failure reason")
	}
}

// A failure on a live ticket is the real incident: the client or the engineers are
// still waiting. It must stay visible, however old it is.
func TestFailedOperationOnLiveTicketIsKept(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := context.Background()
	for _, status := range []domain.Status{domain.New, domain.InProgress, domain.Waiting} {
		if err := s.Transact(ctx, func(tx domain.Tx) error {
			ticket.Status = status
			return tx.SaveTicket(ctx, &ticket)
		}); err != nil {
			t.Fatal(err)
		}
		// Older than the untied retirement window, to show age alone is not enough.
		id := failedKind(t, s, ticket.ID, "client_message", 2*time.Hour)
		retired, err := s.RetireFinishedFailures(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if retired != 0 || state(t, s, id) != "failed" {
			t.Fatalf("failure on %s ticket was hidden: retired=%d state=%s", status, retired, state(t, s, id))
		}
	}
}

// Screens carry no ticket. An old one can never be delivered, but a fresh one is kept
// so an operator still has a chance to see the incident.
func TestUntiedFailureIsRetiredOnlyAfterTheWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := failedKind(t, s, 0, "ui_screen", 30*time.Minute)
	fresh := failedKind(t, s, 0, "ui_screen", time.Minute)

	retired, err := s.RetireFinishedFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("retired %d operations, want only the old one", retired)
	}
	if got := state(t, s, old); got != "done" {
		t.Fatalf("old untied failure kept as %q", got)
	}
	if got := state(t, s, fresh); got != "failed" {
		t.Fatalf("fresh untied failure was hidden as %q", got)
	}
}

// An operation pointing at a ticket that no longer exists is a data problem worth
// looking at, not noise, so it is left alone.
func TestFailureWithDanglingTicketReferenceIsKept(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := failedKind(t, s, 999999, "client_message", 48*time.Hour)
	retired, err := s.RetireFinishedFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("retired %d dangling failures", retired)
	}
	if got := state(t, s, id); got != "failed" {
		t.Fatalf("dangling failure was hidden as %q", got)
	}
}

// Running the sweep with nothing to do must be a no-op, so the minute tick stays cheap.
func TestRetirementIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if retired, err := s.RetireFinishedFailures(ctx); err != nil || retired != 0 {
		t.Fatalf("empty sweep retired %d: %v", retired, err)
	}
	ticket := seed(t, s)
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		ticket.Status = domain.Closed
		return tx.SaveTicket(ctx, &ticket)
	}); err != nil {
		t.Fatal(err)
	}
	failedKind(t, s, ticket.ID, "refresh_card", time.Minute)
	first, err := s.RetireFinishedFailures(ctx)
	if err != nil || first != 1 {
		t.Fatalf("first sweep retired %d: %v", first, err)
	}
	if second, err := s.RetireFinishedFailures(ctx); err != nil || second != 0 {
		t.Fatalf("second sweep retired %d: %v", second, err)
	}
}
