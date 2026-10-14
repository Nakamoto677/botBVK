package analytics

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Explicit opt-in: this publishes a five-year synthetic history only in the
// isolated per-test database, never in the DSN's original database.
func TestFiveYearRefresh(t *testing.T) {
	if os.Getenv("TEST_ANALYTICS_LOAD") != "1" {
		t.Skip("TEST_ANALYTICS_LOAD=1 not set")
	}
	s := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const count = 73000
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO public.bvk_companies(id,name) VALUES(1,'Нагрузочная компания')"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []domain.User{{ID: 100, Name: "Клиент", Role: "client", CompanyID: 1}, {ID: 10, Name: "Инженер", Role: "engineer", Departments: []domain.Department{domain.IT}}} {
		raw, _ := json.Marshal(u)
		if _, err = tx.Exec(ctx, "INSERT INTO public.bvk_users(id,payload) VALUES($1,$2)", u.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	start := func(i int) time.Time { return now.AddDate(0, 0, -i/40).Add(-8 * time.Hour) }
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"public", "bvk_tickets"}, []string{"id", "client_id", "company_id", "status", "department", "version", "payload"}, pgx.CopyFromSlice(count, func(i int) ([]any, error) {
		ticket := domain.Ticket{ID: int64(i + 1), ClientID: 100, CompanyID: 1, Department: domain.IT, Status: domain.Closed, AssigneeID: 10, CreatedAt: start(i), UpdatedAt: start(i).Add(time.Hour)}
		raw, e := json.Marshal(ticket)
		return []any{ticket.ID, 100, 1, "closed", "it", 1, raw}, e
	}))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{"created", "claimed", "message_delivered", "status_changed", "status_changed", "closed"}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"public", "bvk_events"}, []string{"id", "ticket_id", "payload"}, pgx.CopyFromSlice(count*6, func(i int) ([]any, error) {
		ticketID := int64(i/6 + 1)
		kind := kinds[i%6]
		var data any
		switch i % 6 {
		case 2:
			data = map[string]any{"source_message_id": ticketID, "direction": "outbound"}
		case 3:
			data = map[string]string{"status": "waiting_client"}
		case 4:
			data = map[string]string{"status": "in_progress"}
		case 5:
			data = map[string]string{"resolution": "Готово"}
		}
		payload, _ := json.Marshal(data)
		event := domain.Event{ID: int64(i + 1), TicketID: ticketID, ActorID: 10, Kind: kind, CreatedAt: start(i / 6).Add(time.Duration(i%6) * 10 * time.Minute), Data: payload}
		raw, e := json.Marshal(event)
		return []any{event.ID, ticketID, raw}, e
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"public", "bvk_messages"}, []string{"id", "ticket_id", "direction", "payload"}, pgx.CopyFromSlice(count, func(i int) ([]any, error) {
		m := domain.Message{ID: int64(i + 1), TicketID: int64(i + 1), AuthorID: 10, Direction: "outbound", CreatedAt: start(i).Add(15 * time.Minute), Text: "Текст не публикуется"}
		raw, e := json.Marshal(m)
		return []any{m.ID, m.TicketID, m.Direction, raw}, e
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	began := time.Now()
	snapshot, err := s.Refresh(refreshCtx, options())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tickets) != count || snapshot.IncompleteCount != 0 {
		t.Fatalf("tickets=%d incomplete=%d", len(snapshot.Tickets), snapshot.IncompleteCount)
	}
	t.Logf("five-year read + calculation + publication: %s; tickets=%d events=%d messages=%d", time.Since(began), count, count*6, count)
}
