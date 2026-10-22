package postgres

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestWorkflowCleanupIsScopedDurableAndConcurrent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, m := range []domain.AuxiliaryMessage{{UserID: 1, ChatID: -10, TopicID: 5, Scenario: "close", MessageID: 11}, {UserID: 2, ChatID: -10, TopicID: 5, Scenario: "close", MessageID: 12}, {UserID: 1, ChatID: -10, TopicID: 6, Scenario: "close", MessageID: 13}, {UserID: 1, ChatID: -10, TopicID: 5, Scenario: "transfer", MessageID: 14}} {
		if err := s.SaveAuxiliary(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveWorkflow(ctx, 1, -10, 5, "close", []byte(`{"ticket_id":42}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Workflow(ctx, 1, -10, 5, "close")
	var saved struct {
		TicketID int64 `json:"ticket_id"`
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &saved); err != nil || saved.TicketID != 42 {
		t.Fatalf("workflow did not persist %s: %v", raw, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.QueueAuxiliaryCleanup(ctx, 1, -10, 5, "close"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM bvk_outbox WHERE kind='delete_aux_message' AND payload->>'message_id'='11'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cleanup dedup count=%d err=%v", n, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM bvk_auxiliary_messages`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("other flows removed count=%d err=%v", n, err)
	}
	if err = s.DeleteWorkflow(ctx, 1, -10, 5, "close"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Workflow(ctx, 1, -10, 5, "close"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
}
