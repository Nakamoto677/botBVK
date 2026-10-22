package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type durableUIStore struct {
	*adapterStore
	workflows  map[promptKey][]byte
	aux        map[auxKey][]domain.AuxiliaryMessage
	cleanupErr error
}

func (s *durableUIStore) SaveAuxiliary(_ context.Context, m domain.AuxiliaryMessage) error {
	k := auxKey{m.UserID, m.ChatID, m.TopicID, m.Scenario}
	s.aux[k] = append(s.aux[k], m)
	return nil
}
func (s *durableUIStore) QueueAuxiliaryCleanup(_ context.Context, u, c, t int64, scenario string) error {
	if s.cleanupErr != nil {
		return s.cleanupErr
	}
	k := auxKey{u, c, t, scenario}
	for _, m := range s.aux[k] {
		raw, _ := json.Marshal(map[string]int64{"chat_id": c, "message_id": m.MessageID})
		s.operations = append(s.operations, domain.Operation{Kind: "delete_aux_message", Payload: raw})
	}
	delete(s.aux, k)
	return nil
}
func (s *durableUIStore) SaveWorkflow(_ context.Context, u, c, t int64, _ string, p []byte) error {
	s.workflows[promptKey{u, c, t}] = append([]byte{}, p...)
	return nil
}
func (s *durableUIStore) Workflow(_ context.Context, u, c, t int64, _ string) ([]byte, error) {
	p, ok := s.workflows[promptKey{u, c, t}]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return p, nil
}
func (s *durableUIStore) DeleteWorkflow(_ context.Context, u, c, t int64, _ string) error {
	delete(s.workflows, promptKey{u, c, t})
	return nil
}

type cancelUIService struct {
	adapterService
	store   *durableUIStore
	calls   int
	failure error
}

func (s *cancelUIService) Cancel(_ context.Context, _ int64, _ int64) error {
	if s.failure != nil {
		return s.failure
	}
	s.calls++
	s.store.ticket.Status = domain.Cancelled
	return nil
}
func uiClient(t *testing.T) (*Client, *[]map[string]any) {
	t.Helper()
	requests := []map[string]any{}
	id := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		requests = append(requests, p)
		id++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
	}))
	t.Cleanup(server.Close)
	client := NewClient("test")
	client.BaseURL = server.URL
	return client, &requests
}
func clientUIFixture(t *testing.T) (*Adapter, *durableUIStore, *cancelUIService, *Message, *[]map[string]any) {
	client, requests := uiClient(t)
	s := &durableUIStore{adapterStore: &adapterStore{user: domain.User{ID: 1, Role: "client", CompanyID: 10, Approved: true}, ticket: domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, Status: domain.InProgress}}, workflows: map[promptKey][]byte{}, aux: map[auxKey][]domain.AuxiliaryMessage{}}
	service := &cancelUIService{store: s}
	m := &Message{ID: 80, From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}}
	return NewAdapter(client, s, service, Config{}), s, service, m, requests
}
func TestCancelConfirmationDeclineRestartRepeatAndCleanup(t *testing.T) {
	ctx := context.Background()
	a, s, svc, m, requests := clientUIFixture(t)
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last["text"] != "Точно проблема решена?" {
		t.Fatal(last)
	}
	raw, _ := json.Marshal(last["reply_markup"])
	if !strings.Contains(string(raw), "Да, завершить") || !strings.Contains(string(raw), "Нет, продолжить") {
		t.Fatal(string(raw))
	}
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "promptcancel", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	// The question is disposable, but the decline itself must be confirmed:
	// otherwise the screen vanishes and the client cannot tell what happened.
	last = (*requests)[len(*requests)-1]
	if last["text"] != fmt.Sprintf(clientCancelDeclinedText, 7) {
		t.Fatalf("decline left no receipt: %v", last)
	}
	if svc.calls != 0 || len(s.workflows) != 0 || len(s.operations) != 1 {
		t.Fatal("decline did not end scoped flow")
	}
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	// New adapter has empty memory: persisted confirmation must survive restart.
	a = NewAdapter(a.client, s, svc, Config{})
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 || len(s.workflows) != 0 {
		t.Fatal("cancellation requested a reason or persisted stale state")
	}
	if len(s.aux) != 0 {
		t.Fatalf("decline receipt outlived its flow: %v", s.aux)
	}
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 {
		t.Fatal("repeat confirmation repeated mutation")
	}
}
func TestCancelFailureKeepsInputAndCleanupFailureDoesNotRollback(t *testing.T) {
	ctx := context.Background()
	a, s, svc, m, _ := clientUIFixture(t)
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	svc.failure = errors.New("temporary save failure")
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); err == nil {
		t.Fatal("save failure hidden")
	}
	if len(s.workflows) != 1 || len(s.aux) != 1 || len(s.operations) != 0 {
		t.Fatal("failed input cleaned")
	}
	svc.failure = nil
	s.cleanupErr = errors.New("cleanup unavailable")
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 || s.ticket.Status != domain.Cancelled {
		t.Fatal("cleanup rolled back saved cancellation")
	}
	s.cleanupErr = nil
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 1 || len(s.workflows) != 0 || len(s.aux) != 0 {
		t.Fatal("stale flow did not recover after cleanup failure")
	}
}

func currentParts(s *durableUIStore) []string {
	var p prompt
	json.Unmarshal(s.workflows[promptKey{1, 1, 0}], &p)
	return []string{"", "7", p.FlowID}
}

func TestOldConfirmationCannotCancelNewScenarioOrReopenedCycle(t *testing.T) {
	ctx := context.Background()
	a, s, svc, m, _ := clientUIFixture(t)
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	old := currentParts(s)
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, old); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("old button accepted: %v", err)
	}
	s.ticket.RoutingGeneration++
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "cancelconfirm", 7, currentParts(s)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("old cycle accepted: %v", err)
	}
	if svc.calls != 0 {
		t.Fatal("stale confirmation cancelled new cycle")
	}
}

func TestDeclineNoticeDescribesRealTicketState(t *testing.T) {
	ctx := context.Background()
	a, s, svc, m, requests := clientUIFixture(t)
	if err := a.beginCancel(ctx, 1, m, 7); err != nil {
		t.Fatal(err)
	}
	// A colleague may finish the ticket before the client answers the question;
	// the receipt must not promise an open ticket in that case.
	s.ticket.Status = domain.Cancelled
	if err := a.promptCallback(ctx, &Callback{From: Person{ID: 1}, Message: m}, "promptcancel", 7, currentParts(s)); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last["text"] != "Заявка #7 уже завершена — отменять нечего." {
		t.Fatalf("decline misreported ticket state: %v", last)
	}
	if svc.calls != 0 {
		t.Fatal("decline cancelled the ticket")
	}
}

func TestCancelCommandWithoutReasonStartsConfirmation(t *testing.T) {
	a, _, svc, m, requests := clientUIFixture(t)
	m.Text = "/cancel 7"
	if err := a.ticketCommand(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if svc.calls != 0 || (*requests)[len(*requests)-1]["text"] != "Точно проблема решена?" {
		t.Fatal("command bypassed confirmation")
	}
}
