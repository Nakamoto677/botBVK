package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"errors"
	"strings"
	"testing"
)

// quarantineStore adds the optional quarantine capability on top of the shared
// adapter double, so the admin command can be exercised without PostgreSQL.
type quarantineStore struct {
	*adapterStore
	rows   []domain.QuarantinedUpdate
	releas int64
	count  int
	fail   error
}

func (s *quarantineStore) QuarantinedUpdates(context.Context) ([]domain.QuarantinedUpdate, error) {
	return s.rows, s.fail
}
func (s *quarantineStore) QuarantineCount(context.Context) (int, error) {
	return s.count, s.fail
}
func (s *quarantineStore) ReleaseQuarantine(_ context.Context, id int64) error {
	for i, row := range s.rows {
		if row.ID != id {
			continue
		}
		s.releas++
		s.rows = append(s.rows[:i], s.rows[i+1:]...)
		s.count = len(s.rows)
		return nil
	}
	return domain.ErrNotFound
}

func quarantineFixture(t *testing.T, rows []domain.QuarantinedUpdate) (*Adapter, *quarantineStore, *[]map[string]any) {
	t.Helper()
	client, requests := uiClient(t)
	s := &quarantineStore{
		adapterStore: &adapterStore{user: domain.User{ID: 1, Name: "Админ", Role: "admin", Approved: true}},
		rows:         rows,
		count:        len(rows),
	}
	return NewAdapter(client, s, &adapterService{}, Config{}), s, requests
}

func adminMessage(text string) *Message {
	return &Message{ID: 80, From: Person{ID: 1, FirstName: "Админ"}, Chat: Chat{ID: 1, Type: "private"}, Text: text}
}

func sentTexts(t *testing.T, requests *[]map[string]any) []string {
	t.Helper()
	out := []string{}
	for _, r := range *requests {
		if text, ok := r["text"].(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func TestQuarantineCommandNamesAffectedAccountsAndOffersRelease(t *testing.T) {
	adapter, store, requests := quarantineFixture(t, []domain.QuarantinedUpdate{
		{ID: 41, ActorID: 77, ChatID: 77, Attempts: 5, LastError: "Telegram вернул 400: message thread not found"},
		{ID: 42, ActorID: 88, ChatID: -100123, Attempts: 3, LastError: "не удалось отправить экран"},
	})
	if err := adapter.Handle(context.Background(), Update{Message: adminMessage("/quarantine")}); err != nil {
		t.Fatal(err)
	}
	texts := sentTexts(t, requests)
	if len(texts) != 2 {
		t.Fatalf("expected one message per quarantined update, got %d: %v", len(texts), texts)
	}
	// The operator must see which account is frozen and how to free it.
	for i, want := range []string{"41", "77", "5", "/quarantine clear 41"} {
		if !strings.Contains(texts[0], want) {
			t.Fatalf("first message lacks %q (index %d): %q", want, i, texts[0])
		}
	}
	if !strings.Contains(texts[1], "42") || !strings.Contains(texts[1], "-100123") {
		t.Fatalf("second message misidentifies the affected chat: %q", texts[1])
	}
	if store.releas != 0 {
		t.Fatalf("listing released an update: %d", store.releas)
	}
}

func TestQuarantineCommandReportsEmptyState(t *testing.T) {
	adapter, _, requests := quarantineFixture(t, nil)
	if err := adapter.Handle(context.Background(), Update{Message: adminMessage("/quarantine")}); err != nil {
		t.Fatal(err)
	}
	texts := sentTexts(t, requests)
	if len(texts) != 1 || !strings.Contains(texts[0], "пуст") {
		t.Fatalf("empty quarantine not reported clearly: %v", texts)
	}
}

func TestQuarantineClearReleasesOnlyTheNamedUpdate(t *testing.T) {
	adapter, store, requests := quarantineFixture(t, []domain.QuarantinedUpdate{{ID: 41, ActorID: 77, Attempts: 5, LastError: "сбой"}})
	if err := adapter.Handle(context.Background(), Update{Message: adminMessage("/quarantine clear 41")}); err != nil {
		t.Fatal(err)
	}
	if store.releas != 1 {
		t.Fatalf("release not applied: %d", store.releas)
	}
	texts := sentTexts(t, requests)
	if len(texts) != 1 || !strings.Contains(texts[0], "41") {
		t.Fatalf("release confirmation missing: %v", texts)
	}
	// An unknown or already released row is reported, not silently accepted.
	store.fail = nil
	client, requests2 := uiClient(t)
	adapter2 := NewAdapter(client, store, &adapterService{}, Config{})
	if err := adapter2.Handle(context.Background(), Update{Message: adminMessage("/quarantine clear 999")}); err != nil {
		t.Fatal(err)
	}
	texts = sentTexts(t, requests2)
	if len(texts) != 1 || !strings.Contains(texts[0], "не находится в карантине") {
		t.Fatalf("unknown release not reported: %v", texts)
	}
}

func TestQuarantineCommandRequiresAdministrator(t *testing.T) {
	client, requests := uiClient(t)
	clientUser := &adapterStore{user: domain.User{ID: 1, Name: "Админ", Role: "client", Approved: true}}
	adapter := NewAdapter(client, clientUser, &adapterService{}, Config{})
	if err := adapter.Handle(context.Background(), Update{Message: adminMessage("/quarantine")}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-admin reached quarantine: %v", err)
	}
	if len(sentTexts(t, requests)) != 0 {
		t.Fatal("quarantine state leaked to a non-admin")
	}
}
