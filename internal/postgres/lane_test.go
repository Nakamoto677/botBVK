package postgres

import (
	"encoding/json"
	"testing"

	"bot-bvk/internal/domain"
)

// A client notification carries text fixed at commit time and changes no ticket
// state, so it must not wait for topic preparation. Ticket bookkeeping such as
// create_topic and the card refreshes belongs to no chat stream and must not hold
// a lane either: inheriting the ticket's client_id is what used to keep the
// "engineer took your ticket" notice behind the whole topic setup.
func TestClientNotificationDoesNotWaitForTopicPreparation(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := t.Context()
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		for _, op := range []domain.Operation{
			{Kind: "create_topic", TicketID: ticket.ID, Payload: json.RawMessage(`{"routing_generation":1}`)},
			{Kind: "refresh_card", TicketID: ticket.ID, Payload: json.RawMessage(`null`)},
			{Kind: "notify_client", TicketID: ticket.ID, Payload: json.RawMessage(`{"text":"Инженер взял вашу заявку в работу"}`)},
		} {
			if err := tx.Enqueue(ctx, op); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The topic preparation is running, exactly like a real claim in progress.
	topic, err := s.NextOperation(ctx)
	if err != nil || topic.Kind != "create_topic" {
		t.Fatalf("first pick must be the topic: %v %v", topic, err)
	}
	// The notification must already be selectable next to it.
	notice, err := s.NextOperation(ctx)
	if err != nil || notice.Kind != "notify_client" {
		t.Fatalf("notification waited for topic preparation: %v %v", notice, err)
	}
	// While it is leased, the remaining card refresh still must not run ahead of
	// the ticket's own state changes, so nothing else becomes selectable.
	if _, err = s.NextOperation(ctx); err == nil {
		t.Fatal("card refresh overtook topic preparation")
	}
}

// The chat stream keeps its order: a form screen and a notification for the same
// chat cannot swap places, because the client would then read the outcome before
// the screen it belongs to.
func TestFormScreenKeepsItsOrderAgainstClientNotification(t *testing.T) {
	s := testStore(t)
	ticket := seed(t, s)
	ctx := t.Context()
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		for _, op := range []domain.Operation{
			{Kind: "ui_screen", Payload: json.RawMessage(`{"user_id":1,"chat_id":1,"text":"form"}`)},
			{Kind: "notify_client", TicketID: ticket.ID, Payload: json.RawMessage(`{"text":"accepted"}`)},
		} {
			if err := tx.Enqueue(ctx, op); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	screen, err := s.NextOperation(ctx)
	if err != nil || screen.Kind != "ui_screen" {
		t.Fatalf("first pick must be the screen: %v %v", screen, err)
	}
	if _, err = s.NextOperation(ctx); err == nil {
		t.Fatal("notification overtook the form screen of the same chat")
	}
	if err = s.FinishOperation(ctx, screen.ID); err != nil {
		t.Fatal(err)
	}
	notice, err := s.NextOperation(ctx)
	if err != nil || notice.Kind != "notify_client" {
		t.Fatalf("notification did not follow the screen: %v %v", notice, err)
	}
}
