package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

func TestCompanySyncPreservesLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ticket := seed(t, s)
	oldID := ticket.CompanyID
	message := domain.Message{TicketID: ticket.ID, AuthorID: 1, Direction: "inbound", Text: "history", ArchivePath: "archive/original", SHA256: "unchanged"}
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveUser(ctx, domain.User{ID: 2, Role: "client", CompanyID: oldID}); err != nil {
			return err
		}
		if err := tx.RequestAccess(ctx, 2, oldID); err != nil {
			return err
		}
		if err := tx.AddMessage(ctx, &message); err != nil {
			return err
		}
		return tx.AddEvent(ctx, domain.Event{TicketID: ticket.ID, Kind: "historical", Data: json.RawMessage(`{"keep":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	beforeUser, _ := s.User(ctx, 1)
	beforeTicket, _ := s.Ticket(ctx, ticket.ID)
	beforeMessages, _ := s.Messages(ctx, ticket.ID)
	beforeEvents, _ := s.Events(ctx, ticket.ID)
	beforeAccess, _ := s.AccessRequests(ctx)
	defs := []domain.CompanyDefinition{{Key: "original", Name: "Renamed", LegacyName: "Example"}, {Key: "added", Name: "Added"}}
	for range 2 {
		if err := s.BootstrapCompanies(ctx, 99, defs); err != nil {
			t.Fatal(err)
		}
	}
	companies, err := s.Companies(ctx)
	if err != nil || len(companies) != 2 || companies[0].ID != oldID || companies[0].Name != "Renamed" {
		t.Fatalf("rename/add: %#v %v", companies, err)
	}
	addedID := companies[1].ID
	// Same stable key, another name, and reordered list retain identity.
	defs = []domain.CompanyDefinition{{Key: "added", Name: "Added renamed"}, {Key: "original", Name: "Renamed again"}}
	if err := s.BootstrapCompanies(ctx, 99, defs); err != nil {
		t.Fatal(err)
	}
	companies, _ = s.Companies(ctx)
	if companies[0].ID != addedID || companies[1].ID != oldID {
		t.Fatal("position changed identity")
	}
	// Replacement may even reuse the display name but must get a different ID.
	defs = []domain.CompanyDefinition{{Key: "replacement", Name: "Renamed again"}, {Key: "added", Name: "Added renamed"}}
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.BootstrapCompanies(ctx, 99, defs); err != nil {
			t.Fatal(err)
		}
	}
	companies, _ = s.Companies(ctx)
	if len(companies) != 2 || companies[0].ID == oldID {
		t.Fatal("replacement reused identity")
	}
	historical, err := s.Company(ctx, oldID)
	if err != nil || historical.Name != "Renamed again" {
		t.Fatalf("historical company lookup: %#v %v", historical, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_companies").Scan(&count); err != nil || count != 3 {
		t.Fatalf("duplicates/deletion: %d %v", count, err)
	}
	afterUser, _ := s.User(ctx, 1)
	afterTicket, _ := s.Ticket(ctx, ticket.ID)
	afterMessages, _ := s.Messages(ctx, ticket.ID)
	afterEvents, _ := s.Events(ctx, ticket.ID)
	afterAccess, _ := s.AccessRequests(ctx)
	for _, pair := range [][2]any{{beforeUser, afterUser}, {beforeTicket, afterTicket}, {beforeMessages, afterMessages}, {beforeEvents, afterEvents}, {beforeAccess, afterAccess}} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("history changed: %#v -> %#v", pair[0], pair[1])
		}
	}
	if err := tickets.New(s).Register(ctx, 3, "New", oldID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("stale registration: %v", err)
	}
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveUser(ctx, domain.User{ID: 3, Role: "client", CompanyID: oldID}); err != nil {
			return err
		}
		return tx.RequestAccess(ctx, 3, oldID)
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("transaction guard: %v", err)
	}
	if _, err := s.User(ctx, 3); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rejected registration left user")
	}
	if err := tickets.New(s).Register(ctx, 4, "New", companies[0].ID); err != nil {
		t.Fatal(err)
	}
	// Existing approved client can still create tickets after exclusion.
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		if err := tx.SaveUser(ctx, domain.User{ID: 5, Role: "client", Approved: true, CompanyID: oldID}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.New(s).Create(ctx, 5, domain.IT, "still supported"); err != nil {
		t.Fatalf("access revoked: %v", err)
	}
	// Invalid explicit migration rolls back deactivation and partial insertions.
	snapshot, _ := s.Companies(ctx)
	if err := s.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "partial", Name: "Partial"}, {Key: "bad", Name: "Bad", LegacyName: "does not exist"}}); err == nil {
		t.Fatal("missing legacy source accepted")
	}
	after, _ := s.Companies(ctx)
	if !reflect.DeepEqual(snapshot, after) {
		t.Fatal("failed sync changed catalog")
	}
}

func TestLegacyCompanyMigrationAndStringReplacement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ticket := seed(t, s)
	// Reconstruct the original catalog schema and rerun the real migration.
	if _, err := s.pool.Exec(ctx, `DELETE FROM bvk_migrations WHERE version='007_company_sync'; ALTER TABLE bvk_companies DROP COLUMN config_key; ALTER TABLE bvk_companies DROP COLUMN registration_active; ALTER TABLE bvk_companies DROP COLUMN registration_order; ALTER TABLE bvk_companies ADD CONSTRAINT bvk_companies_name_key UNIQUE(name)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "original", Name: "New name", LegacyName: "Example"}}); err != nil {
		t.Fatal(err)
	}
	companies, _ := s.Companies(ctx)
	if len(companies) != 1 || companies[0].ID != ticket.CompanyID {
		t.Fatal("legacy adoption lost identity")
	}
	for range 2 {
		if err := s.Bootstrap(ctx, 99, []string{"Changed string"}); err != nil {
			t.Fatal(err)
		}
	}
	companies, _ = s.Companies(ctx)
	if len(companies) != 1 || companies[0].ID == ticket.CompanyID {
		t.Fatal("string rename guessed identity")
	}
	if err := s.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "wrong", Name: "Wrong", LegacyName: "New name"}}); err == nil {
		t.Fatal("adopted stable company as legacy")
	}
	if err := s.Bootstrap(ctx, 99, []string{}); err != nil {
		t.Fatal(err)
	}
	companies, _ = s.Companies(ctx)
	if len(companies) != 0 {
		t.Fatal("empty catalog ignored")
	}
	got, _ := s.Ticket(ctx, ticket.ID)
	if got.CompanyID != ticket.CompanyID {
		t.Fatal("historical company changed")
	}
}

func TestCompanyMigrationRejectsAmbiguousAndRepeatedSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	before, _ := s.Companies(ctx)
	defs := []domain.CompanyDefinition{domain.LegacyCompany("Example"), {Key: "new", Name: "Renamed", LegacyName: "Example"}}
	if err := s.BootstrapCompanies(ctx, 99, defs); err == nil {
		t.Fatal("same row adopted twice")
	}
	after, _ := s.Companies(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("conflict partially applied")
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO bvk_companies(name) VALUES('Ambiguous'),('Ambiguous')"); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "ambiguous", Name: "Renamed", LegacyName: "Ambiguous"}}); err == nil {
		t.Fatal("ambiguous source accepted")
	}
	after, _ = s.Companies(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("ambiguous migration changed catalog")
	}
}
