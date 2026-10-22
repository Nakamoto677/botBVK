package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

// Opt-in destructive synthetic work is confined to testStore's disposable schema.
func TestPrereleaseConcurrentLoad(t *testing.T) {
	if os.Getenv("TEST_PRERELEASE_LOAD") != "1" {
		t.Skip("TEST_PRERELEASE_LOAD=1 required")
	}
	s := testStore(t)
	ctx := t.Context()
	companies, err := s.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const users, concurrency = 1000, 32
	if err = s.Transact(ctx, func(tx domain.Tx) error {
		for i := range users {
			if e := tx.SaveUser(ctx, domain.User{ID: int64(1000 + i), CompanyID: companies[0].ID, Role: "client", Approved: true}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := tickets.New(s)
	run := func(name string, n int, fn func(int) error) {
		t.Helper()
		jobs := make(chan int, n)
		for i := range n {
			jobs <- i
		}
		close(jobs)
		latencies := make([]time.Duration, n)
		var wg sync.WaitGroup
		started := time.Now()
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					at := time.Now()
					if e := fn(i); e != nil {
						t.Errorf("%s[%d]: %v", name, i, e)
					}
					latencies[i] = time.Since(at)
				}
			}()
		}
		wg.Wait()
		duration := time.Since(started)
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		t.Logf("%s: requests=%d concurrency=%d elapsed=%s rate=%.1f/s p50=%s p95=%s p99=%s", name, n, concurrency, duration, float64(n)/duration.Seconds(), latencies[n/2], latencies[n*95/100], latencies[n*99/100])
		if t.Failed() {
			t.FailNow()
		}
	}
	var created atomic.Int64
	run("create with duplicate submissions", users*2, func(i int) error {
		_, e := svc.Create(ctx, int64(1000+i%users), domain.IT, "Synthetic pre-release load")
		if e == nil {
			created.Add(1)
			return nil
		}
		if errors.Is(e, domain.ErrConflict) {
			return nil
		}
		return e
	})
	if created.Load() != users {
		t.Fatalf("created=%d want=%d", created.Load(), users)
	}
	var accepted atomic.Int64
	run("durable inbox duplicate delivery", 10000, func(i int) error {
		id := int64(i%5000 + 1)
		raw := json.RawMessage(fmt.Sprintf(`{"update_id":%d,"message":{"from":{"id":%d},"chat":{"id":%d,"type":"private"},"text":"load"}}`, id, 1000+i%users, 1000+i%users))
		ok, e := s.SaveUpdate(ctx, id, raw)
		if ok {
			accepted.Add(1)
		}
		return e
	})
	if accepted.Load() != 5000 {
		t.Fatalf("accepted=%d want=5000", accepted.Load())
	}
	var total, active, events, operations int
	for _, check := range []struct {
		query  string
		target *int
		want   int
	}{
		{"SELECT count(*) FROM bvk_tickets", &total, users},
		{"SELECT count(DISTINCT client_id) FROM bvk_tickets WHERE status='new'", &active, users},
		{"SELECT count(*) FROM bvk_events WHERE payload->>'Kind'='created'", &events, users},
	} {
		if e := s.pool.QueryRow(ctx, check.query).Scan(check.target); e != nil {
			t.Fatal(e)
		}
		if *check.target != check.want {
			t.Fatalf("%s: got=%d want=%d", check.query, *check.target, check.want)
		}
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox").Scan(&operations); err != nil {
		t.Fatal(err)
	}
	seen := sync.Map{}
	var drained atomic.Int64
	run("concurrent outbox leasing and acknowledgement", concurrency, func(_ int) error {
		for {
			op, e := s.NextOperation(ctx)
			if errors.Is(e, domain.ErrNotFound) {
				return nil
			}
			if e != nil {
				return e
			}
			if _, duplicate := seen.LoadOrStore(op.ID, true); duplicate {
				return fmt.Errorf("duplicate lease %d", op.ID)
			}
			if e = s.FinishOperation(ctx, op.ID); e != nil {
				return e
			}
			drained.Add(1)
		}
	})
	if drained.Load() != int64(operations) {
		t.Fatalf("drained=%d queued=%d", drained.Load(), operations)
	}
	t.Logf("verified: tickets=%d unique_clients=%d created_events=%d unique_updates=%d outbox_completed=%d", total, active, events, accepted.Load(), drained.Load())
}
