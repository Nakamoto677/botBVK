package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("analytics_test_%d", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		base.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); _, _ = base.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); base.Close() })
	botSchema, err := os.ReadFile("../postgres/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(botSchema)); err != nil {
		t.Fatal(err)
	}
	s := Store{Pool: pool}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func fixture(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	when := time.Now().UTC().Add(-2 * time.Hour)
	users := []domain.User{{ID: 100, Name: "Клиент", Role: "client", Approved: true, CompanyID: 1}, {ID: 10, Name: "Инженер", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO public.bvk_companies(id,name) VALUES(1,'Компания')"); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		b, _ := json.Marshal(u)
		if _, err := s.Pool.Exec(ctx, "INSERT INTO public.bvk_users VALUES($1,$2)", u.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	ticket := domain.Ticket{ID: 1, ClientID: 100, CompanyID: 1, Department: domain.IT, Status: domain.Closed, AssigneeID: 10, CreatedAt: when, UpdatedAt: when.Add(time.Hour), ClosedBy: 10, Description: "Описание"}
	b, _ := json.Marshal(ticket)
	if _, err := s.Pool.Exec(ctx, "INSERT INTO public.bvk_tickets(id,client_id,company_id,status,department,version,payload) VALUES(1,100,1,'closed','it',1,$1)", b); err != nil {
		t.Fatal(err)
	}
	for i, e := range []domain.Event{
		{TicketID: 1, ActorID: 100, Kind: "created", CreatedAt: when, Data: json.RawMessage("null")},
		{TicketID: 1, ActorID: 10, Kind: "claimed", CreatedAt: when.Add(time.Minute), Data: json.RawMessage("null")},
		{TicketID: 1, ActorID: 10, Kind: "message_delivered", CreatedAt: when.Add(10 * time.Minute), Data: json.RawMessage(`{"source_message_id":1,"direction":"outbound"}`)},
		{TicketID: 1, ActorID: 10, Kind: "closed", CreatedAt: when.Add(time.Hour), Data: json.RawMessage(`{"resolution":"Готово"}`)},
	} {
		e.ID = int64(i + 1)
		b, _ := json.Marshal(e)
		if _, err := s.Pool.Exec(ctx, "INSERT INTO public.bvk_events(id,ticket_id,payload) VALUES($1,1,$2)", e.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	m := domain.Message{ID: 1, TicketID: 1, AuthorID: 10, Direction: "outbound", CreatedAt: when.Add(5 * time.Minute), Text: "PRIVATE MESSAGE MUST NOT BE PUBLISHED", FileID: "PRIVATE FILE"}
	b, _ = json.Marshal(m)
	if _, err := s.Pool.Exec(ctx, "INSERT INTO public.bvk_messages(id,ticket_id,direction,payload) VALUES(1,1,'outbound',$1)", b); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshAtomicFailureAndRestart(t *testing.T) {
	s := integrationStore(t)
	fixture(t, s)
	ctx := context.Background()
	got, err := s.Refresh(ctx, options())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tickets) != 1 || got.IncompleteCount != 0 || got.Cycles[0].FirstResponseBy == nil || *got.Cycles[0].FirstResponseBy != 10 {
		t.Fatalf("snapshot %+v", got)
	}
	var client string
	var response float64
	if err = s.Pool.QueryRow(ctx, "SELECT client_name,first_response_work_seconds FROM analytics.tickets WHERE id=1").Scan(&client, &response); err != nil {
		t.Fatal(err)
	}
	if client != "Клиент" {
		t.Fatal(client)
	}
	// A reader using a stable transaction retains the old version while publish
	// replaces every table. New readers receive the new version.
	reader, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	var before time.Time
	if err = reader.QueryRow(ctx, "SELECT snapshot_at FROM analytics.refresh_state").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, options()); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err = reader.QueryRow(ctx, "SELECT snapshot_at FROM analytics.refresh_state").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatal("reader saw partial replacement")
	}
	reader.Rollback(ctx)
	// Fail after DELETE has already executed, during the COPY stage.
	if _, err = s.Pool.Exec(ctx, "ALTER TABLE analytics.tickets ADD CONSTRAINT forced_failure CHECK(id<0) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, options()); err == nil {
		t.Fatal("expected publication failure")
	}
	var count int
	var failed string
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM analytics.tickets").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("previous snapshot lost", count)
	}
	if err = s.Pool.QueryRow(ctx, "SELECT last_error FROM analytics.refresh_state WHERE id=1").Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed == "" {
		t.Fatal("missing failure marker")
	}
	if _, err = s.Pool.Exec(ctx, "ALTER TABLE analytics.tickets DROP CONSTRAINT forced_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, options()); err != nil {
		t.Fatal("restart refresh", err)
	}
	if err = s.Pool.QueryRow(ctx, "SELECT last_error FROM analytics.refresh_state WHERE id=1").Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != "" {
		t.Fatal("failure not cleared")
	}
	// Raw message text never enters the published schema.
	var published string
	if err = s.Pool.QueryRow(ctx, "SELECT string_agg(row_to_json(t)::text,'') FROM analytics.tickets t").Scan(&published); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(published, "PRIVATE") {
		t.Fatal("message body leaked")
	}
}

func TestSingletonAndRepeatableSource(t *testing.T) {
	s := integrationStore(t)
	fixture(t, s)
	ctx := context.Background()
	held, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err = held.Exec(ctx, "SELECT pg_advisory_xact_lock(731904)"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, options()); err != ErrBusy {
		t.Fatalf("lock err=%v", err)
	}
	held.Rollback(ctx)
	reader, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	first, err := readSource(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "UPDATE public.bvk_users SET payload=jsonb_set(payload,'{name}','\"Changed\"') WHERE id=100"); err != nil {
		t.Fatal(err)
	}
	second, err := readSource(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	if first.Users[1].Name != second.Users[1].Name {
		t.Fatal("source snapshot not consistent")
	}
}

func TestMetabasePermissions(t *testing.T) {
	s := integrationStore(t)
	fixture(t, s)
	ctx := context.Background()
	if _, err := s.Refresh(ctx, options()); err != nil {
		t.Fatal(err)
	}
	role := fmt.Sprintf("analytics_reader_%d", time.Now().UnixNano())
	if _, err := s.Pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN; GRANT USAGE ON SCHEMA analytics TO "+role+"; GRANT SELECT ON ALL TABLES IN SCHEMA analytics TO "+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Pool.Exec(ctx, "DROP OWNED BY "+role+"; DROP ROLE "+role) })
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "RESET ROLE")
	if _, err = conn.Exec(ctx, "SELECT * FROM analytics.tickets"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"SELECT * FROM public.bvk_tickets", "UPDATE analytics.tickets SET description='overwrite'", "DELETE FROM analytics.cycles", "CREATE TABLE analytics.forbidden(id int)"} {
		if _, err = conn.Exec(ctx, q); err == nil {
			t.Fatal("forbidden query allowed", q)
		}
	}
}

func TestAnalyticsDoesNotShareInboxArchiveFence(t *testing.T) {
	s := integrationStore(t)
	fixture(t, s)
	ctx := t.Context()
	held, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	// Inbox acceptance takes this shared fence. It must not suppress analytics.
	if _, err = held.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(731903)"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Refresh(ctx, options()); err != nil {
		t.Fatalf("analytics collided with inbox: %v", err)
	}
}
