package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/monitor"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/telegram"
	"bot-bvk/internal/tickets"
	"github.com/jackc/pgx/v5"
)

// claimBaseline measures where the wall-clock time of "engineer claims a ticket"
// actually goes, using the production rate-limiter setting. It separates the delay
// this code imposes on itself from the time Telegram takes, which is the split that
// decides whether speeding up is a matter of our scheduler or of the network.
//
// The numbers are printed, not asserted: they are a baseline to compare later
// changes against, and a slow CI machine must not fail the suite.
func TestClaimBaselineBreakdown(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(ctx)
	name := fmt.Sprintf("bvk_baseline_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	store, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, 99, []string{"Client company"}); err != nil {
		t.Fatal(err)
	}
	companies, _ := store.Companies(ctx)
	svc := tickets.New(store)
	if err = svc.Register(ctx, 1, "Client", companies[0].ID); err != nil {
		t.Fatal(err)
	}
	requests, _ := store.AccessRequests(ctx)
	if err = svc.ApproveAccess(ctx, 99, requests[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		return tx.SaveUser(ctx, domain.User{ID: 2, Name: "Engineer", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}})
	}); err != nil {
		t.Fatal(err)
	}

	// A mock that answers like Telegram does, so the measured HTTP time is realistic
	// instead of instantaneous. 250 ms is in the range the plan targets.
	const telegramLatency = 250 * time.Millisecond
	var calls []string
	var mu sync.Mutex
	counter := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		calls = append(calls, method)
		counter++
		id := counter
		mu.Unlock()
		time.Sleep(telegramLatency)
		result := any(true)
		switch method {
		case "createForumTopic":
			result = map[string]any{"message_thread_id": id}
		case "sendMessage":
			result = map[string]any{"message_id": id, "chat": map[string]any{"id": 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()

	client := telegram.NewClient("test")
	client.BaseURL = server.URL
	cfg := telegram.Config{Departments: map[domain.Department]telegram.Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}}, ArchiveDir: t.TempDir()}
	scheduler := &monitor.Scheduler{Store: store, Config: config.Config{AdminID: 99, BackupDir: t.TempDir(), ArchiveDir: cfg.ArchiveDir}, Calendar: tickets.DefaultCalendar()}
	cfg.BeforeDelete = scheduler.BeforeDelete
	worker := telegram.NewWorker(client, store, cfg)

	drain := func() {
		t.Helper()
		time.Sleep(50 * time.Millisecond)
		for range 30 {
			op, e := store.NextOperation(ctx)
			if e != nil {
				return
			}
			if e = worker.Handle(ctx, op); e != nil {
				t.Fatalf("operation %s failed: %v", op.Kind, e)
			}
			if e = store.FinishOperation(ctx, op.ID); e != nil {
				t.Fatal(e)
			}
		}
	}

	ticket, err := svc.Create(ctx, 1, domain.IT, "Baseline problem")
	if err != nil {
		t.Fatal(err)
	}
	drain()

	started := time.Now()
	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	var topicReadyAt, clientToldAt, topicPickedAt, clientPickedAt time.Duration
	// Drive the queue the way the real worker does: up to four operations in
	// flight, picked through NextOperationExcluding. Draining one operation at a
	// time would hide the very effect under measurement, because a single-runner
	// drain serialises everything by construction.
	const slots = 4
	type finished struct {
		id   int64
		kind string
		at   time.Duration
	}
	results := make(chan finished, 16)
	inFlight := map[int64]bool{}
	running := 0
	deadline := time.After(60 * time.Second)
	for {
		if running < slots {
			active := make([]int64, 0, len(inFlight))
			for id := range inFlight {
				active = append(active, id)
			}
			op, e := store.NextOperationExcluding(ctx, active)
			if e == nil {
				running++
				inFlight[op.ID] = true
				switch op.Kind {
				case "create_topic":
					topicPickedAt = time.Since(started)
				case "notify_client":
					clientPickedAt = time.Since(started)
				}
				go func(op domain.Operation) {
					handleErr := worker.Handle(ctx, op)
					if handleErr == nil {
						handleErr = store.FinishOperation(ctx, op.ID)
					}
					if handleErr != nil {
						results <- finished{id: op.ID, kind: "error", at: time.Since(started)}
						t.Errorf("operation %s failed: %v", op.Kind, handleErr)
						return
					}
					results <- finished{id: op.ID, kind: op.Kind, at: time.Since(started)}
				}(op)
				continue
			}
		}
		if running == 0 {
			break
		}
		select {
		case f := <-results:
			running--
			delete(inFlight, f.id)
			switch f.kind {
			case "create_topic":
				topicReadyAt = f.at
			case "notify_client":
				clientToldAt = f.at
			}
		case <-deadline:
			t.Fatal("queue did not settle")
		case <-time.After(20 * time.Millisecond):
		}
	}
	total := time.Since(started)

	// Per-kind durations come from the worker's own instrumentation.
	rendered := client.Metrics.Render()
	kindSeconds := map[string]string{}
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, "bvk_operation_s_max{") {
			continue
		}
		var label, value string
		if _, err = fmt.Sscanf(line, `bvk_operation_s_max{label=%q} %s`, &label, &value); err == nil {
			kindSeconds[label] = value
		}
	}
	var limiterMS, httpMS, httpMax string
	for _, line := range strings.Split(rendered, "\n") {
		switch {
		case strings.HasPrefix(line, `bvk_ratelimit_wait_ms_sum{label="sendMessage"}`):
			limiterMS = strings.Fields(line)[1]
		case strings.HasPrefix(line, `bvk_telegram_call_ms_sum{label="sendMessage"}`):
			httpMS = strings.Fields(line)[1]
		case strings.HasPrefix(line, `bvk_telegram_call_ms_max{label="createForumTopic"}`):
			httpMax = strings.Fields(line)[1]
		}
	}
	mu.Lock()
	callCount := len(calls)
	sequence := strings.Join(calls, " → ")
	mu.Unlock()

	t.Logf("claim total wall clock: %v", total.Round(10*time.Millisecond))
	t.Logf("topic ready for the engineers at: %v", topicReadyAt.Round(10*time.Millisecond))
	t.Logf("client told the ticket was taken at: %v", clientToldAt.Round(10*time.Millisecond))
	t.Logf("create_topic picked at %v, notify_client picked at %v", topicPickedAt.Round(10*time.Millisecond), clientPickedAt.Round(10*time.Millisecond))
	t.Logf("telegram calls during claim: %d", callCount)
	t.Logf("call sequence: %s", sequence)
	t.Logf("rate-limiter wait total: %s ms (this is the delay the bot imposes on itself)", limiterMS)
	t.Logf("sendMessage HTTP total: %s ms over a mock with %v latency per call", httpMS, telegramLatency)
	t.Logf("createForumTopic HTTP max: %s ms", httpMax)
	for _, kind := range []string{"create_topic", "refresh_card", "notify_client"} {
		if v, ok := kindSeconds[kind]; ok {
			t.Logf("operation %-16s max duration: %s s", kind, v)
		}
	}

	// The claim must still produce exactly one topic and reach the client.
	after, err := store.Ticket(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.TopicID == 0 || after.AssigneeID != 2 {
		t.Fatalf("claim did not provision the ticket: %+v", after)
	}
	if callCount < 3 {
		t.Fatalf("claim made only %d telegram calls", callCount)
	}
}
