package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

func TestCompanyCodeBindingAndTransfer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "alpha", Name: "Alpha"}, {Key: "beta", Name: "Beta"}}); err != nil {
		t.Fatal(err)
	}
	companies, err := s.Companies(ctx)
	if err != nil || len(companies) != 2 {
		t.Fatal(companies, err)
	}
	first, second := companies[0], companies[1]
	svc := tickets.NewWithCompanyCodes(s, map[string]int64{"00000000000000000000000000": first.ID, "11111111111111111111111111": second.ID})
	for id := int64(1); id <= 50; id++ {
		company, err := svc.CompanyForCode(ctx, id, "00000-00000-00000-00000-000000")
		if err != nil || company.ID != first.ID {
			t.Fatal(company, err)
		}
		if err := svc.BindCompany(ctx, id, "Employee", company.ID); err != nil {
			t.Fatalf("employee %d: %v", id, err)
		}
	}
	if err := svc.Register(ctx, 51, "Waiting employee", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindCompany(ctx, 51, "Waiting employee", first.ID); err != nil {
		t.Fatal(err)
	}
	var pendingStatus string
	if err := s.pool.QueryRow(ctx, "SELECT status FROM bvk_access WHERE user_id=51 AND method='admin'").Scan(&pendingStatus); err != nil || pendingStatus != "superseded" {
		t.Fatal("old pending request was not superseded", pendingStatus, err)
	}
	rotated := tickets.NewWithCompanyCodes(s, map[string]int64{"22222222222222222222222222": first.ID, "11111111111111111111111111": second.ID})
	if _, err := rotated.CompanyForCode(ctx, 52, "00000000000000000000000000"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("replaced code remained valid", err)
	}
	if _, err := rotated.CompanyForCode(ctx, 52, "22222222222222222222222222"); err != nil {
		t.Fatal("replacement code rejected", err)
	}
	if existing, err := s.User(ctx, 2); err != nil || !existing.Approved {
		t.Fatal("code rotation revoked an existing user", existing, err)
	}
	if err := s.PutDraft(ctx, 1, domain.Draft{Department: domain.IT, Step: "description", Text: "old company draft"}); err != nil {
		t.Fatal(err)
	}
	ticket, err := svc.Create(ctx, 1, domain.IT, "old company ticket")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BindCompany(ctx, 1, "Employee", second.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("active ticket did not block transfer: %v", err)
	}
	if err := svc.Cancel(ctx, 1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindCompany(ctx, 1, "Employee", second.ID); err != nil {
		t.Fatal(err)
	}
	u, err := s.User(ctx, 1)
	if err != nil || !u.Approved || u.CompanyID != second.ID {
		t.Fatal(u, err)
	}
	if _, err := s.LoadDraft(ctx, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("old draft survived transfer", err)
	}
	old, err := s.Ticket(ctx, ticket.ID)
	if err != nil || old.CompanyID != first.ID || old.Status != domain.Cancelled {
		t.Fatal("old ticket was reassigned", old, err)
	}
	var bindings int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_access WHERE user_id=1 AND method='company_code' AND status='approved'").Scan(&bindings); err != nil || bindings != 2 {
		t.Fatal("binding audit missing", bindings, err)
	}
	for range 5 {
		_, err = svc.CompanyForCode(ctx, 88, "WRONG")
	}
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatal("invalid attempts did not trigger cooldown", err)
	}
	if _, err = svc.CompanyForCode(ctx, 88, "00000000000000000000000000"); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatal("valid code bypassed cooldown", err)
	}
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		u, e := tx.GetUser(ctx, 1)
		if e != nil {
			return e
		}
		u.Approved = false
		return tx.SaveUser(ctx, u)
	}); err != nil {
		t.Fatal(err)
	}
	if user, err := s.User(ctx, 1); err != nil || user.Approved {
		t.Fatal("individual revoke failed", user, err)
	}
}

func TestFinishedCompanyCodeUpdateIsRedacted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const input = `{"update_id":987654,"message":{"message_id":1,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"/start join_00000000000000000000000000"}}`
	if _, err := s.SaveUpdate(ctx, 987654, json.RawMessage(input)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSensitiveUpdate(ctx, 987654); err != nil {
		t.Fatal(err)
	}
	var raw string
	var finished bool
	if err := s.pool.QueryRow(ctx, "SELECT payload::text,finished FROM bvk_updates WHERE id=987654").Scan(&raw, &finished); err != nil {
		t.Fatal(err)
	}
	if !finished || strings.Contains(raw, "00000000000000000000000000") {
		t.Fatal("credential remained in completed inbox update")
	}
}
