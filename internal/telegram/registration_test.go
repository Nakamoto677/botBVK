package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

type registrationStore struct {
	*adapterStore
	companies []domain.Company
}

func (s *registrationStore) Companies(context.Context) ([]domain.Company, error) {
	return s.companies, nil
}
func (s *registrationStore) User(context.Context, int64) (domain.User, error) {
	return domain.User{}, domain.ErrNotFound
}

type registrationService struct {
	Service
	calls int
}

func (s *registrationService) Register(context.Context, int64, string, int64) error {
	s.calls++
	return domain.ErrInvalid
}

func TestRegistrationFreshListAndStaleButtons(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "empty"}[empty], func(t *testing.T) {
			client, capture := capturedClient()
			store := &registrationStore{adapterStore: &adapterStore{}, companies: []domain.Company{{ID: 20, Name: "Новое название"}}}
			if empty {
				store.companies = nil
			}
			svc := &registrationService{}
			a := NewAdapter(client, store, svc, Config{})
			m := &Message{ID: 1, Chat: Chat{ID: 7, Type: "private"}, From: Person{ID: 7, FirstName: "Клиент"}, Text: "/start"}
			if err := a.Handle(context.Background(), Update{Message: m}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(capture.bodies)
			if !empty && (!strings.Contains(string(raw), "Новое название") || !strings.Contains(string(raw), "company:20")) {
				t.Fatal(string(raw))
			}
			if empty && !strings.Contains(string(raw), "Сейчас нет компаний") {
				t.Fatal(string(raw))
			}
			// A restart cannot make the retired numeric callback valid again.
			a = NewAdapter(client, store, svc, Config{})
			start := len(capture.calls)
			if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "stale", From: m.From, Message: m, Data: "company:10"}}); err != nil {
				t.Fatal(err)
			}
			if svc.calls != 0 {
				t.Fatal("stale callback reached registration")
			}
			if !strings.HasSuffix(capture.calls[start], "answerCallbackQuery") {
				t.Fatal("tap not acknowledged first")
			}
			raw, _ = json.Marshal(capture.bodies[start:])
			if strings.Contains(string(raw), "company:10") {
				t.Fatal("retired button still offered")
			}
			if !empty && !strings.Contains(string(raw), "актуального списка") {
				t.Fatal(string(raw))
			}
			// If exclusion races with the selection, the service rejection also refreshes.
			if !empty {
				if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "race", From: m.From, Message: m, Data: "company:20"}}); err != nil {
					t.Fatal(err)
				}
				if svc.calls != 1 {
					t.Fatal("valid choice did not reach guarded service")
				}
			}
		})
	}
}

type historicalCompanyStore struct{ *registrationStore }

func (s *historicalCompanyStore) Company(_ context.Context, id int64) (domain.Company, error) {
	return domain.Company{ID: id, Name: "Исключённая компания"}, nil
}
func TestHistoricalCompanyNameAvailableOutsideRegistration(t *testing.T) {
	store := &historicalCompanyStore{&registrationStore{adapterStore: &adapterStore{}}}
	name, err := companyName(context.Background(), store, 10)
	if err != nil || name != "Исключённая компания" {
		t.Fatalf("name=%q err=%v", name, err)
	}
}
