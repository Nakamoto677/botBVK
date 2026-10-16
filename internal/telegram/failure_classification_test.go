package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/tickets"
	"github.com/jackc/pgx/v5"
)

// failureDatabase builds its own schema and returns a direct connection to that exact
// schema. Guessing the schema by name is unsafe: another test may have created one with
// the same prefix and the assertions would then read the wrong tables.
func failureDatabase(t *testing.T) (*postgres.Store, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("failure_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		base.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		base.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return store, conn
}

// seedClientAndEngineer prepares one approved client and one engineer, which is the
// minimum for a claimable ticket in a migrated database.
func seedClientAndEngineer(t *testing.T, store *postgres.Store) *tickets.Service {
	t.Helper()
	ctx := context.Background()
	svc := tickets.New(store)
	if err := store.Bootstrap(ctx, 99, []string{"Client company"}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	companies, err := store.Companies(ctx)
	if err != nil {
		t.Fatalf("companies: %v", err)
	}
	if err = svc.Register(ctx, 1, "Client", companies[0].ID); err != nil {
		t.Fatalf("register: %v", err)
	}
	requests, err := store.AccessRequests(ctx)
	if err != nil {
		t.Fatalf("requests: %v", err)
	}
	if err = svc.ApproveAccess(ctx, 99, requests[0].ID, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		return tx.SaveUser(ctx, domain.User{ID: 2, Name: "Engineer", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}})
	}); err != nil {
		t.Fatalf("engineer: %v", err)
	}
	return svc
}

// telegramUnavailable is a mock that never answers, which is what a blocked network looks
// like from inside the process: DNS resolves, the connection never completes.
func telegramUnavailable(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// Each case is a failure path the operator has to live with. What matters is that the bot
// neither crashes nor silently drops work: the operation either stays retryable or is
// recorded as dead with a reason an operator can act on.
func TestFailurePathsClassifyOperationsConsistently(t *testing.T) {
	cases := []struct {
		name string
		// response is the body Telegram sends. Empty means "never answers".
		response string
		// wantState is the outbox state after one attempt.
		wantState string
		// wantScheduledForward is true when the work must come back for another attempt.
		wantScheduledForward bool
	}{
		{
			name:                 "client blocked the bot",
			response:             `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`,
			wantState:            "failed",
			wantScheduledForward: false,
		},
		{
			name:                 "rate limited with retry_after",
			response:             `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`,
			wantState:            "pending",
			wantScheduledForward: true,
		},
		{
			name:                 "invalid token",
			response:             `{"ok":false,"error_code":401,"description":"Unauthorized"}`,
			wantState:            "failed",
			wantScheduledForward: false,
		},
		{
			name:                 "telegram unreachable",
			response:             "",
			wantState:            "pending",
			wantScheduledForward: true,
		},
		{
			name:                 "telegram server error",
			response:             `{"ok":false,"error_code":500,"description":"Internal Server Error"}`,
			wantState:            "pending",
			wantScheduledForward: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, conn := failureDatabase(t)
			ctx := context.Background()
			cfg := Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}}
			svc := seedClientAndEngineer(t, store)
			ticket, err := svc.Create(ctx, 1, domain.IT, "Failure path "+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
				t.Fatal(err)
			}

			var server *httptest.Server
			if tc.response == "" {
				server = telegramUnavailable(t)
			} else {
				body := tc.response
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(body))
				}))
			}
			client := NewClient("test")
			client.BaseURL = server.URL
			// Keep the unreachable-network case quick: the production client waits 65 s,
			// which would dominate the suite. The path under test is the classification,
			// not the length of the timeout.
			client.HTTP.Timeout = 300 * time.Millisecond
			worker := NewWorker(client, store, cfg)
			worker.interval = 0

			op, err := store.NextOperation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// runOperation owns the retry policy and must return nil on every path here: a
			// non-nil error tears down the worker loop, which is what must never happen.
			if err = worker.runOperation(ctx, op); err != nil {
				t.Fatalf("the worker returned a fatal error on %s: %v", tc.name, err)
			}

			var state, reason string
			var availableAt time.Time
			if err = conn.QueryRow(ctx, `SELECT state, last_error, available_at FROM bvk_outbox WHERE id=$1`, op.ID).Scan(&state, &reason, &availableAt); err != nil {
				t.Fatal(err)
			}
			if state != tc.wantState {
				t.Fatalf("state=%q, want %q (reason: %s)", state, tc.wantState, reason)
			}
			if reason == "" {
				t.Fatal("the failure left no reason for the operator")
			}
			if strings.Contains(reason, client.Token) {
				t.Fatal("the stored reason contains the bot token")
			}
			if tc.wantScheduledForward {
				if !availableAt.After(time.Now()) {
					t.Fatalf("retryable operation was not scheduled forward: available_at=%v", availableAt)
				}
				return
			}
			// A permanent failure must be counted, or /ready would stay green.
			var failed int
			if err = conn.QueryRow(ctx, `SELECT count(*) FROM bvk_outbox WHERE state='failed'`).Scan(&failed); err != nil {
				t.Fatal(err)
			}
			if failed == 0 {
				t.Fatal("a permanent failure was not counted, so readiness would stay green")
			}
		})
	}
}

// A rate limit must respect the delay Telegram asked for, not our own backoff, otherwise
// we keep hitting an API that explicitly asked us to stop.
func TestRateLimitUsesTheRequestedRetryDelay(t *testing.T) {
	store, conn := failureDatabase(t)
	ctx := context.Background()
	svc := seedClientAndEngineer(t, store)
	ticket, err := svc.Create(ctx, 1, domain.IT, "Rate limited")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":45}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	cfg := Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}}
	worker := NewWorker(client, store, cfg)
	worker.interval = 0

	op, err := store.NextOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.runOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	var availableAt time.Time
	if err = conn.QueryRow(ctx, `SELECT available_at FROM bvk_outbox WHERE id=$1`, op.ID).Scan(&availableAt); err != nil {
		t.Fatal(err)
	}
	// Telegram asked for 45 s. Our own backoff on the first attempt is 1 s, so a wait
	// clearly above that proves the requested value was honoured.
	wait := time.Until(availableAt)
	if wait < 40*time.Second {
		t.Fatalf("retry scheduled in %v, Telegram asked for 45s", wait.Round(time.Second))
	}
	if wait > 60*time.Second {
		t.Fatalf("retry scheduled too far out: %v", wait.Round(time.Second))
	}
}
