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

func TestCleanupWaitsForFailedArchivesAndRetries(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	var m = domain.Message{TicketID: v.ID, Direction: "inbound", FileID: "f", FileSize: 100}
	if err := s.Transact(ctx, func(tx domain.Tx) error { return tx.AddMessage(ctx, &m) }); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bvk_outbox(kind,ticket_id,payload,state,attempts) VALUES('archive_message',$1,jsonb_build_object('ID',$2::bigint),'failed',12)`, v.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := s.CleanupSettled(ctx, v.ID)
	if err != nil || settled {
		t.Fatalf("unarchived failure settled=%v err=%v", settled, err)
	}
	if err = s.CleanupRetry(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var state string
	if err = s.pool.QueryRow(ctx, `SELECT state,attempts FROM bvk_outbox WHERE kind='archive_message'`).Scan(&state, &attempts); err != nil || state != "pending" || attempts != 0 {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestCleanupFencesAcceptedInboxUntilProcessed(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	if _, err := s.SaveUpdate(ctx, 100, json.RawMessage(`{"update_id":100,"edited_message":{"message_id":9}}`)); err != nil {
		t.Fatal(err)
	}
	settled, err := s.CleanupSettled(ctx, v.ID)
	if err != nil || settled {
		t.Fatalf("pending inbox ignored: %v %v", settled, err)
	}
	err = s.Transact(ctx, func(tx domain.Tx) error { return s.CleanupFence(ctx, tx, v.ID) })
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("unprocessed edit not fenced: %v", err)
	}
	if err = s.FinishUpdate(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(ctx, func(tx domain.Tx) error { return s.CleanupFence(ctx, tx, v.ID) }); err != nil {
		t.Fatal(err)
	}
}
