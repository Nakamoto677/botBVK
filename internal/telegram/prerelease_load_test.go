package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

func TestPrereleaseTenRequestsPerSecond(t *testing.T) {
	if os.Getenv("TEST_PRERELEASE_LOAD") != "1" {
		t.Skip("TEST_PRERELEASE_LOAD=1 required")
	}
	store, conn := failureDatabase(t)
	seedClientAndEngineer(t, store)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	companies, err := store.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		for i := range 100 {
			if e := tx.SaveUser(ctx, domain.User{ID: int64(1000 + i), Role: "client", Approved: true, CompanyID: companies[0].ID}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": sent.Add(1)}})
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	a := NewAdapter(client, store, tickets.New(store), Config{})
	worker := NewWorker(client, store, Config{}) // Keep the production rate limiter and four worker slots.
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	const paced, burst = 600, 400
	latencies := make([]time.Duration, 0, paced+burst)
	started := time.Now()
	for i := range paced + burst {
		if i < paced {
			delay := time.Until(started.Add(time.Duration(i) * 100 * time.Millisecond))
			if delay > 0 && !pause(ctx, delay) {
				t.Fatal(ctx.Err())
			}
		}
		at := time.Now()
		id := int64(1000 + i%100)
		u := Update{ID: int64(i + 1), Message: &Message{ID: int64(i + 1), From: Person{ID: id, FirstName: "Load"}, Chat: Chat{ID: id, Type: "private"}, Text: "/help"}}
		raw, e := json.Marshal(u)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = store.SaveUpdate(ctx, u.ID, raw); e != nil {
			t.Fatal(e)
		}
		if e = a.processUpdate(ctx, domain.StoredUpdate{ID: u.ID, Payload: raw}); e != nil {
			t.Fatal(e)
		}
		latencies = append(latencies, time.Since(at))
	}
	inputElapsed := time.Since(started)
	for {
		var unfinished, failed int
		if err = conn.QueryRow(ctx, "SELECT count(*) FILTER (WHERE state IN ('pending','processing')),count(*) FILTER (WHERE state='failed') FROM bvk_outbox").Scan(&unfinished, &failed); err != nil {
			t.Fatal(err)
		}
		if failed != 0 {
			t.Fatalf("failed operations: %d", failed)
		}
		if unfinished == 0 {
			break
		}
		if !pause(ctx, 100*time.Millisecond) {
			t.Fatal(ctx.Err())
		}
	}
	var finished, quarantined int
	if err = conn.QueryRow(ctx, "SELECT count(*) FILTER (WHERE finished),count(*) FILTER (WHERE quarantined) FROM bvk_updates").Scan(&finished, &quarantined); err != nil {
		t.Fatal(err)
	}
	if finished != paced+burst || quarantined != 0 || sent.Load() < paced+burst {
		t.Fatalf("finished=%d quarantined=%d sends=%d", finished, quarantined, sent.Load())
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("100 users; 600 updates at 10/s + 400 burst; input=%s total=%s drain=%s processing_p95=%s processing_p99=%s API_calls=%d; real limiter, mock HTTP=20ms", inputElapsed, time.Since(started), time.Since(started)-inputElapsed, latencies[len(latencies)*95/100], latencies[len(latencies)*99/100], sent.Load())
	t.Log(fmt.Sprintf("finished=%d quarantined=%d", finished, quarantined))
}
