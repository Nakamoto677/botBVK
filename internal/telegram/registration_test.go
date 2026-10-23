package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

type registrationStore struct {
	*adapterStore
	companies []domain.Company
}

type companyUIStore struct {
	*adapterStore
	users  map[int64]domain.User
	active domain.Ticket
}

func (s *companyUIStore) Companies(context.Context) ([]domain.Company, error) {
	return []domain.Company{{ID: 10, Name: "Alpha"}, {ID: 20, Name: "Beta"}}, nil
}
func (s *companyUIStore) User(_ context.Context, id int64) (domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return u, domain.ErrNotFound
	}
	return u, nil
}
func (s *companyUIStore) ActiveTicket(_ context.Context, id int64) (domain.Ticket, error) {
	if s.active.ClientID == id && s.active.Status.Active() {
		return s.active, nil
	}
	return domain.Ticket{}, domain.ErrNotFound
}

type companyUIService struct {
	Service
	store   *companyUIStore
	version string
}

func (s *companyUIService) CompanyCodeVersion(int64) string {
	if s.version != "" {
		return s.version
	}
	return "initial-version"
}

func (s *companyUIService) CompanyForCode(_ context.Context, _ int64, code string) (domain.Company, error) {
	code, _ = domain.NormalizeCompanyCode(code)
	switch code {
	case "00000000000000000000000000":
		return domain.Company{ID: 10, Name: "Alpha"}, nil
	case "11111111111111111111111111":
		return domain.Company{ID: 20, Name: "Beta"}, nil
	default:
		return domain.Company{}, domain.ErrInvalid
	}
}
func (s *companyUIService) BindCompany(_ context.Context, id int64, name string, companyID int64) error {
	if s.store.active.Status.Active() {
		return domain.ErrConflict
	}
	s.store.users[id] = domain.User{ID: id, Name: name, Role: "client", Approved: true, CompanyID: companyID}
	return nil
}

func TestCompanyCodeLinkAndSelfTransfer(t *testing.T) {
	ctx := context.Background()
	client, capture := capturedClient()
	store := &companyUIStore{adapterStore: &adapterStore{}, users: map[int64]domain.User{}}
	service := &companyUIService{store: store}
	a := NewAdapter(client, store, service, Config{})
	manual := &Message{ID: 2, Chat: Chat{ID: 8, Type: "private"}, From: Person{ID: 8, FirstName: "Second"}, Text: "/start"}
	if err := a.Handle(ctx, Update{Message: manual}); err != nil {
		t.Fatal(err)
	}
	manual.Text = "00000-00000-00000-00000-000000"
	if err := a.Handle(ctx, Update{Message: manual}); err != nil {
		t.Fatal(err)
	}
	manualPrompt := a.prompts[scope(8, manual)]
	if err := a.Handle(ctx, Update{Callback: &Callback{ID: "manual", From: manual.From, Message: manual, Data: fmt.Sprintf("bindcompany:10:%s", manualPrompt.FlowID)}}); err != nil || store.users[8].CompanyID != 10 {
		t.Fatal("manual first-run code did not authorize client", err)
	}
	m := &Message{ID: 1, Chat: Chat{ID: 7, Type: "private"}, From: Person{ID: 7, FirstName: "Client"}, Text: "/start join_00000000000000000000000000"}
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(capture.bodies)
	if !strings.Contains(string(raw), "Подключиться к компании «Alpha»") || strings.Contains(string(raw), "00000000000000000000000000") {
		t.Fatal("link did not open a safe confirmation", string(raw))
	}
	p := a.prompts[scope(7, m)]
	confirm := &Callback{ID: "bind", From: m.From, Message: m, Data: fmt.Sprintf("bindcompany:10:%s", p.FlowID)}
	service.version = "rotated-version"
	if err := a.Handle(ctx, Update{Callback: confirm}); err != nil || store.users[7].Approved {
		t.Fatal("old code confirmation survived rotation", err)
	}
	service.version = ""
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	p = a.prompts[scope(7, m)]
	confirm.Data = fmt.Sprintf("bindcompany:10:%s", p.FlowID)
	if err := a.Handle(ctx, Update{Callback: confirm}); err != nil || store.users[7].CompanyID != 10 {
		t.Fatal("link did not authorize client", err, store.users[7])
	}
	if err := a.Handle(ctx, Update{Callback: confirm}); err != nil || store.users[7].CompanyID != 10 {
		t.Fatal("stale callback changed company", err)
	}
	store.active = domain.Ticket{ID: 5, ClientID: 7, CompanyID: 10, Status: domain.New}
	m.Text = "/start join_11111111111111111111111111"
	if err := a.Handle(ctx, Update{Message: m}); err != nil || store.users[7].CompanyID != 10 {
		t.Fatal("active ticket did not block transfer", err)
	}
	store.active.Status = domain.Cancelled
	m.Text = "/company"
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(ctx, Update{Callback: &Callback{ID: "change", From: m.From, Message: m, Data: "companychange"}}); err != nil {
		t.Fatal(err)
	}
	a.drafts[7] = domain.Draft{Step: "description", Text: "unfinished"}
	m.Text = "11111-11111-11111-11111-111111"
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(capture.bodies)
	if !strings.Contains(string(raw), "Незавершённая заявка будет удалена после подтверждения") || store.users[7].CompanyID != 10 {
		t.Fatal("draft warning missing or transfer happened before confirmation")
	}
	p = a.prompts[scope(7, m)]
	if err := a.Handle(ctx, Update{Callback: &Callback{ID: "cancel", From: m.From, Message: m, Data: fmt.Sprintf("cancelcompany:20:%s", p.FlowID)}}); err != nil || store.users[7].CompanyID != 10 || a.drafts[7].Text == "" {
		t.Fatal("cancelling transfer changed the company or draft", err)
	}
	if err := a.Handle(ctx, Update{Callback: &Callback{ID: "change-again", From: m.From, Message: m, Data: "companychange"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	p = a.prompts[scope(7, m)]
	if err := a.Handle(ctx, Update{Callback: &Callback{ID: "switch", From: m.From, Message: m, Data: fmt.Sprintf("bindcompany:20:%s", p.FlowID)}}); err != nil || store.users[7].CompanyID != 20 {
		t.Fatal("manual code did not transfer client", err, store.users[7])
	}
}

func (s *registrationStore) Companies(context.Context) ([]domain.Company, error) {
	return s.companies, nil
}
func (s *registrationStore) User(context.Context, int64) (domain.User, error) {
	return domain.User{}, domain.ErrNotFound
}

func TestRegistrationAsksForCodeAndRetiresCompanyButtons(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "updated", true: "empty"}[empty], func(t *testing.T) {
			client, capture := capturedClient()
			store := &registrationStore{adapterStore: &adapterStore{}, companies: []domain.Company{{ID: 20, Name: "Новое название"}}}
			if empty {
				store.companies = nil
			}
			a := NewAdapter(client, store, nil, Config{})
			m := &Message{ID: 1, Chat: Chat{ID: 7, Type: "private"}, From: Person{ID: 7, FirstName: "Клиент"}, Text: "/start"}
			if err := a.Handle(context.Background(), Update{Message: m}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(capture.bodies)
			if !empty && (!strings.Contains(string(raw), "Введите код") || strings.Contains(string(raw), "company:20") || strings.Contains(string(raw), "Новое название")) {
				t.Fatal(string(raw))
			}
			if empty && !strings.Contains(string(raw), "подключение новых компаний недоступно") {
				t.Fatal(string(raw))
			}
			// A saved numeric company button cannot grant access after the change.
			a = NewAdapter(client, store, nil, Config{})
			start := len(capture.calls)
			if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "stale", From: m.From, Message: m, Data: "company:10"}}); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(capture.calls[start], "answerCallbackQuery") {
				t.Fatal("tap not acknowledged first")
			}
			raw, _ = json.Marshal(capture.bodies[start:])
			if strings.Contains(string(raw), "company:10") {
				t.Fatal("retired button still offered")
			}
			if !empty && !strings.Contains(string(raw), "Введите код") {
				t.Fatal(string(raw))
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
