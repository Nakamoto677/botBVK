package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

// Unlike the permissive capture mock, this fake enforces Telegram's rule:
// messages carrying reply keyboards/removal/ForceReply cannot be edited.
type editableCardAPI struct {
	messages    map[int64]map[string]any
	next        int64
	denied      int
	editFailure int
}

func (a *editableCardAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	var p map[string]any
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, err
	}
	result := `{"ok":true,"result":true}`
	switch {
	case strings.HasSuffix(r.URL.Path, "/createForumTopic"):
		result = `{"ok":true,"result":{"message_thread_id":10}}`
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		a.next++
		a.messages[a.next] = p
		result = fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, a.next)
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		id := int64(p["message_id"].(float64))
		old := a.messages[id]
		markup, _ := old["reply_markup"].(map[string]any)
		if a.editFailure != 0 {
			result = fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"temporary failure"}`, a.editFailure)
		} else if markup["remove_keyboard"] != nil || markup["keyboard"] != nil || markup["force_reply"] != nil {
			a.denied++
			result = `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`
		} else {
			a.messages[id] = p
		}
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result))}, nil
}

func strictCardWorker(t *testing.T, ticket domain.Ticket) (*Worker, *workerStore, *editableCardAPI) {
	t.Helper()
	api := &editableCardAPI{messages: map[int64]map[string]any{}, next: 100}
	client := NewClient("test")
	client.HTTP = &http.Client{Transport: api}
	store := &workerStore{current: ticket}
	w := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100123, QueueTopicID: 1}}})
	w.interval = 0
	return w, store, api
}

func TestEditableCardsThroughClaimRefreshAndClose(t *testing.T) {
	ticket := domain.Ticket{ID: 14, ClientID: 12, Department: domain.IT, Status: domain.New}
	w, store, api := strictCardWorker(t, ticket)
	ctx := context.Background()
	if err := w.Handle(ctx, domain.Operation{ID: 330, Kind: "refresh_card", TicketID: 14}); err != nil {
		t.Fatal(err)
	}
	store.current.Status, store.current.AssigneeID, store.current.RoutingPending = domain.InProgress, 2, true
	for _, op := range []domain.Operation{{ID: 331, Kind: "create_topic", TicketID: 14}, {ID: 332, Kind: "refresh_card", TicketID: 14}} {
		if err := w.Handle(ctx, op); err != nil {
			t.Fatal(op.Kind, err)
		}
	}
	if store.current.RoutingPending || store.current.TopicMessageID == 0 {
		t.Fatal(store.current)
	}
	store.current.Status = domain.Waiting
	if err := w.Handle(ctx, domain.Operation{ID: 333, Kind: "refresh_card", TicketID: 14}); err != nil {
		t.Fatal(err)
	}
	store.current.Status = domain.Closed
	if err := w.Handle(ctx, domain.Operation{ID: 334, Kind: "refresh_card", TicketID: 14}); err != nil {
		t.Fatal(err)
	}
	if err := w.Handle(ctx, domain.Operation{ID: 335, Kind: "close_topic", TicketID: 14, Payload: json.RawMessage(`{"text":"Заявка #14 закрыта"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Handle(ctx, domain.Operation{ID: 336, Kind: "refresh_card", TicketID: 14}); err != nil {
		t.Fatal(err)
	}
	if api.denied != 0 {
		t.Fatal("new flow tried to edit a non-editable message")
	}
	for _, id := range []int64{store.current.TopicMessageID, store.current.QueueMessageID} {
		raw, _ := json.Marshal(api.messages[id]["reply_markup"])
		if string(raw) != `{}` {
			t.Fatal("closed card retains controls", string(raw))
		}
	}
}

func TestBothFailedOperationsRecoverLegacyCard(t *testing.T) {
	for _, firstKind := range []string{"create_topic", "refresh_card"} {
		t.Run(firstKind, func(t *testing.T) {
			ticket := domain.Ticket{ID: 14, ClientID: 12, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, TopicChatID: -100123, TopicID: 10, TopicMessageID: 80, QueueMessageID: 70}
			w, store, api := strictCardWorker(t, ticket)
			api.messages[80] = map[string]any{"text": "original card", "reply_markup": map[string]any{"remove_keyboard": true}}
			for _, kind := range []string{firstKind, "create_topic", "refresh_card", "refresh_card"} {
				if err := w.Handle(context.Background(), domain.Operation{ID: 331, Kind: kind, TicketID: 14}); err != nil {
					t.Fatal(kind, err)
				}
			}
			if store.current.TopicID != 10 || store.current.TopicMessageID != 101 || api.next != 101 || api.denied != 1 {
				t.Fatal("replacement was lost or duplicated", store.current, api)
			}
			if api.messages[80]["text"] != "original card" {
				t.Fatal("original history changed")
			}
			queue, _ := json.Marshal(api.messages[70]["reply_markup"])
			if !strings.Contains(string(queue), "/101?thread=10") {
				t.Fatal("queue link still points to broken card", string(queue))
			}
			if len(store.events) != 1 || !strings.Contains(string(store.events[0].Data), `"replaced_message_id":80`) {
				t.Fatal("replacement not recorded", store.events)
			}
		})
	}
}

type failingCardSaveStore struct {
	*workerStore
	fail bool
}
type failingCardSaveTx struct{ *workerTx }

func (tx *failingCardSaveTx) SaveTicket(context.Context, *domain.Ticket) error {
	return errors.New("DB write failed")
}
func (s *failingCardSaveStore) Transact(ctx context.Context, f func(domain.Tx) error) error {
	if s.fail {
		s.fail = false
		return f(&failingCardSaveTx{&workerTx{store: s.workerStore}})
	}
	return s.workerStore.Transact(ctx, f)
}

func TestReplacementSurvivesDBWriteFailure(t *testing.T) {
	ticket := domain.Ticket{ID: 14, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100123, TopicID: 10, TopicMessageID: 80, QueueMessageID: 70}
	w, store, api := strictCardWorker(t, ticket)
	w.store = &failingCardSaveStore{workerStore: store, fail: true}
	api.messages[80] = map[string]any{"reply_markup": map[string]any{"remove_keyboard": true}}
	op := domain.Operation{ID: 332, Kind: "refresh_card", TicketID: 14}
	if err := w.Handle(context.Background(), op); err == nil {
		t.Fatal("expected DB failure")
	}
	if store.current.TopicMessageID != 80 {
		t.Fatal("failed write changed binding")
	}
	store.current.Status = domain.Closed
	if err := w.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if api.next != 101 || store.current.TopicMessageID != 101 {
		t.Fatal("retry duplicated replacement", api, store.current)
	}
	raw, _ := json.Marshal(api.messages[101]["reply_markup"])
	if string(raw) != `{}` || !strings.Contains(api.messages[101]["text"].(string), "Закрыта") {
		t.Fatal("retried replacement retained controls from before closure", api.messages[101])
	}
}

func TestTransientEditErrorDoesNotReplaceCard(t *testing.T) {
	ticket := domain.Ticket{ID: 14, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100123, TopicID: 10, TopicMessageID: 80}
	w, store, api := strictCardWorker(t, ticket)
	api.editFailure = 429
	if err := w.Handle(context.Background(), domain.Operation{ID: 332, Kind: "refresh_card", TicketID: 14}); err == nil {
		t.Fatal("temporary error swallowed")
	}
	if api.next != 100 || store.current.TopicMessageID != 80 {
		t.Fatal("temporary error created a duplicate card")
	}
}
