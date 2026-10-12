package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"bot-bvk/internal/domain"
)

func TestLegacyQueueOnClaimMigration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ticket := seed(t, s)
	// Recreate pre-upgrade state after the fixture's initial schema setup.
	if _, err := s.pool.Exec(ctx, "DELETE FROM bvk_migrations WHERE version IN ('002_queue_on_claim','003_refresh_cards')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE bvk_tickets SET topic_chat_id=-100,topic_id=22,payload=payload||'{"topic_chat_id":-100,"topic_id":22,"routing_pending":true,"emergency":true}'::jsonb WHERE id=$1`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	var pending, delivered domain.Message
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		pending = domain.Message{TicketID: ticket.ID, AuthorID: 1, Direction: "inbound", Text: "retained pending body", TelegramChatID: 1, TelegramMessageID: 400, ArchivePath: "archives/preserved", SHA256: "preserved-hash", Revisions: []domain.MessageRevision{{Text: "earlier body"}}}
		if err := tx.AddMessage(ctx, &pending); err != nil {
			return err
		}
		delivered = domain.Message{TicketID: ticket.ID, AuthorID: 1, Direction: "inbound", Text: "already delivered", TelegramChatID: 1, TelegramMessageID: 401}
		if err := tx.AddMessage(ctx, &delivered); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"source_message_id": delivered.ID, "telegram_chat_id": -100})
		if err := tx.AddEvent(ctx, domain.Event{TicketID: ticket.ID, Kind: "message_delivered", Data: data}); err != nil {
			return err
		}
		if err := tx.AddEvent(ctx, domain.Event{TicketID: ticket.ID, Kind: "emergency", Data: json.RawMessage(`{"original":true}`)}); err != nil {
			return err
		}
		for _, op := range []domain.Operation{
			{Kind: "create_topic", TicketID: ticket.ID},
			{Kind: "team_message", TicketID: ticket.ID},
			{Kind: "dispatcher_message", Payload: json.RawMessage(`{"text":"old dispatcher"}`)},
			{Kind: "access_notification", Payload: json.RawMessage(`{"text":"Аварийная заявка #1 в отделе it. Откройте очередь и возьмите в работу."}`)},
			{Kind: "access_notification", Payload: json.RawMessage(`{"text":"Доступ одобрен. Вы можете создать заявку."}`)},
			{Kind: "archive_message", TicketID: ticket.ID},
		} {
			if err := tx.Enqueue(ctx, op); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A leased legacy operation is terminalized together with pending ones.
	if _, err := s.pool.Exec(ctx, "UPDATE bvk_outbox SET state='processing',lease_until=now()+interval '5 minutes' WHERE kind='dispatcher_message'"); err != nil {
		t.Fatal(err)
	}
	var beforeEvents, beforeDelivered []byte
	if err := s.pool.QueryRow(ctx, "SELECT jsonb_agg(payload ORDER BY id) FROM bvk_events WHERE ticket_id=$1", ticket.ID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT payload FROM bvk_messages WHERE id=$1", delivered.ID).Scan(&beforeDelivered); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Ticket(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RoutingPending || updated.TopicID != 22 || updated.TopicChatID != -100 || updated.Version != ticket.Version+1 {
		t.Fatalf("legacy ticket not reconciled: %#v", updated)
	}
	var hasEmergency bool
	var rowVersion, payloadVersion int64
	if err := s.pool.QueryRow(ctx, "SELECT payload ? 'emergency',version,(payload->>'version')::bigint FROM bvk_tickets WHERE id=$1", ticket.ID).Scan(&hasEmergency, &rowVersion, &payloadVersion); err != nil {
		t.Fatal(err)
	}
	if hasEmergency || rowVersion != payloadVersion {
		t.Fatalf("ticket migration version/key: %v %d %d", hasEmergency, rowVersion, payloadVersion)
	}
	var deferred bool
	if err := s.pool.QueryRow(ctx, "SELECT (payload->>'deferred_delivery')::boolean FROM bvk_messages WHERE id=$1", pending.ID).Scan(&deferred); err != nil || !deferred {
		t.Fatalf("pending replay marker: %v %v", deferred, err)
	}
	msgs, err := s.Messages(ctx, ticket.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("retained messages: %#v %v", msgs, err)
	}
	if msgs[0].Text != pending.Text || msgs[0].ArchivePath != pending.ArchivePath || msgs[0].SHA256 != pending.SHA256 || len(msgs[0].Revisions) != 1 {
		t.Fatalf("message history/archive changed: %#v", msgs[0])
	}
	var afterEvents, afterDelivered []byte
	if err := s.pool.QueryRow(ctx, "SELECT jsonb_agg(payload ORDER BY id) FROM bvk_events WHERE ticket_id=$1", ticket.ID).Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT payload FROM bvk_messages WHERE id=$1", delivered.ID).Scan(&afterDelivered); err != nil {
		t.Fatal(err)
	}
	if string(beforeEvents) != string(afterEvents) || string(beforeDelivered) != string(afterDelivered) {
		t.Fatal("historical events or delivered message were modified")
	}
	rows, err := s.pool.Query(ctx, "SELECT kind,state,payload FROM bvk_outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	pauses := 0
	for rows.Next() {
		var kind, state string
		var payload json.RawMessage
		if err := rows.Scan(&kind, &state, &payload); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "create_topic", "team_message", "dispatcher_message":
			if state != "done" {
				t.Errorf("obsolete %s still %s", kind, state)
			}
		case "access_notification":
			var p struct{ Text string }
			json.Unmarshal(payload, &p)
			if p.Text == "Доступ одобрен. Вы можете создать заявку." {
				if state != "pending" {
					t.Error("access approval notification was retired")
				}
			} else if state != "done" {
				t.Error("emergency notification still pending")
			}
		case "archive_message":
			if state != "pending" {
				t.Error("archive work was retired")
			}
		case "close_topic":
			pauses++
			var p struct {
				ChatID  int64 `json:"chat_id"`
				TopicID int64 `json:"topic_id"`
				Pause   bool  `json:"pause"`
			}
			if err := json.Unmarshal(payload, &p); err != nil || !p.Pause || p.ChatID != -100 || p.TopicID != 22 {
				t.Errorf("legacy pause payload: %s %v", payload, err)
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if pauses != 1 {
		t.Fatalf("pause count %d", pauses)
	}
	// Current-version changes must survive subsequent startup migrations.
	if _, err := s.pool.Exec(ctx, "UPDATE bvk_tickets SET payload=jsonb_set(payload,'{routing_pending}','true'::jsonb) WHERE id=$1", ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.Ticket(ctx, ticket.ID)
	if err != nil || unchanged.Version != updated.Version || !unchanged.RoutingPending {
		t.Fatalf("migration ran twice: %#v %v", unchanged, err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox WHERE kind='close_topic'").Scan(&pauses); err != nil || pauses != 1 {
		t.Fatalf("pause requeued: %d %v", pauses, err)
	}
}

func TestLegacyAssignedRoutingPreserved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ticket := seed(t, s)
	if _, err := s.pool.Exec(ctx, "DELETE FROM bvk_migrations WHERE version IN ('002_queue_on_claim','003_refresh_cards')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE bvk_tickets SET status='in_progress',payload=payload||'{"status":"in_progress","assignee_id":99,"routing_pending":true,"emergency":false}'::jsonb WHERE id=$1`, ticket.ID); err != nil {
		t.Fatal(err)
	}
	msg := domain.Message{TicketID: ticket.ID, Direction: "inbound", AuthorID: 1, Text: "assigned routing detail"}
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.AddMessage(ctx, &msg); err != nil {
			return err
		}
		return tx.Enqueue(ctx, domain.Operation{Kind: "create_topic", TicketID: ticket.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Ticket(ctx, ticket.ID)
	if err != nil || got.Status != domain.InProgress || got.AssigneeID != 99 || !got.RoutingPending {
		t.Fatalf("assigned routing changed: %#v %v", got, err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, "SELECT state FROM bvk_outbox WHERE kind='create_topic'").Scan(&state); err != nil || state != "pending" {
		t.Fatalf("assigned topic operation retired: %s %v", state, err)
	}
	var deferred bool
	if err := s.pool.QueryRow(ctx, "SELECT payload ? 'deferred_delivery' FROM bvk_messages WHERE id=$1", msg.ID).Scan(&deferred); err != nil || deferred {
		t.Fatalf("assigned message was deferred: %v %v", deferred, err)
	}
}

func TestLegacyCardsRefreshedOnceIncludingClosed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := seed(t, s)
	if _, err := s.pool.Exec(ctx, "DELETE FROM bvk_migrations WHERE version IN ('002_queue_on_claim','003_refresh_cards')"); err != nil {
		t.Fatal(err)
	}
	first.QueueMessageID = 10
	var assigned, closed, invisible domain.Ticket
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveTicket(ctx, &first); err != nil {
			return err
		}
		for _, id := range []int64{2, 3, 4} {
			if err := tx.SaveUser(ctx, domain.User{ID: id, Role: "client", Approved: true, CompanyID: first.CompanyID}); err != nil {
				return err
			}
		}
		assigned = domain.Ticket{ClientID: 2, CompanyID: first.CompanyID, Department: domain.IT, Status: domain.InProgress, AssigneeID: 99, TopicMessageID: 20}
		closed = domain.Ticket{ClientID: 3, CompanyID: first.CompanyID, Department: domain.IT, Status: domain.Closed, QueueMessageID: 30, TopicMessageID: 31}
		invisible = domain.Ticket{ClientID: 4, CompanyID: first.CompanyID, Department: domain.IT, Status: domain.New}
		for _, ticket := range []*domain.Ticket{&assigned, &closed, &invisible} {
			if err := tx.SaveTicket(ctx, ticket); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, "SELECT ticket_id,state FROM bvk_outbox WHERE kind='refresh_card' ORDER BY ticket_id")
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		got[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(got) != 3 || got[first.ID] != "pending" || got[assigned.ID] != "pending" || got[closed.ID] != "pending" {
		t.Fatalf("legacy refresh selection: %#v", got)
	}
	if _, exists := got[invisible.ID]; exists {
		t.Fatal("ticket without existing cards refreshed")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox WHERE kind='refresh_card'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("card refresh repeated: %d %v", count, err)
	}
}

func TestCardRefreshMigrationRunsAfterRoutingUpgrade(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	v := seed(t, s)
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		t, err := tx.GetTicket(ctx, v.ID)
		if err != nil {
			return err
		}
		t.QueueMessageID = 88
		return tx.SaveTicket(ctx, &t)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM bvk_migrations WHERE version='003_refresh_cards'"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox WHERE kind='refresh_card' AND ticket_id=$1", v.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one card refresh after earlier routing upgrade, got %d", count)
	}
}

func TestCompletionMigrationRetainsHistoricalTopics(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	v := seed(t, s)
	if _, err := s.pool.Exec(ctx, `DELETE FROM bvk_migrations WHERE version='004_completion_cleanup'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.AddEvent(ctx, domain.Event{TicketID: v.ID, Kind: "completion_cleanup_requested", Data: json.RawMessage(`{"completion_key":"new-intent","chat_id":-10,"topic_id":9}`)}); err != nil {
			return err
		}
		for _, raw := range []string{`{"chat_id":-10,"topic_id":8}`, `{"completion_key":"new-intent","chat_id":-10,"topic_id":9}`} {
			if err := tx.Enqueue(ctx, domain.Operation{TicketID: v.ID, Kind: "delete_topic", Payload: json.RawMessage(raw)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var historical, current string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM bvk_outbox WHERE kind='delete_topic' AND payload->>'topic_id'='8'`).Scan(&historical); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT state FROM bvk_outbox WHERE kind='delete_topic' AND payload->>'topic_id'='9'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if historical != "done" || current != "pending" {
		t.Fatalf("historical=%s new=%s", historical, current)
	}
}
