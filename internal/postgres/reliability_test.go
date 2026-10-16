package postgres

import (
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestQuarantineSurvivesRestartAndKeepsArchiveFence(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	for _, u := range []struct {
		id  int64
		raw string
	}{{1, `{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"first"}}`}, {2, `{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"second"}}`}, {3, `{"message":{"from":{"id":2},"chat":{"id":2,"type":"private"},"text":"independent"}}`}} {
		if _, err := s.SaveUpdate(ctx, u.id, json.RawMessage(u.raw)); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingUpdates(ctx, 100)
	if err != nil || len(pending) != 2 || pending[0].ID != 1 || pending[1].ID != 3 {
		t.Fatalf("ordered inbox: %v %v", pending, err)
	}
	if err = s.RetryUpdate(ctx, 1, "permanent screen failure", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	// A quarantined update must not order anyone. It can never be selected and
	// nothing clears the flag, so blocking its actor's lane would be permanent
	// and invisible. The dependent update 2 has to become selectable again.
	restarted := &Store{s.pool}
	pending, err = restarted.PendingUpdates(ctx, 100)
	if err != nil || len(pending) != 2 || pending[0].ID != 2 || pending[1].ID != 3 {
		t.Fatalf("quarantine still blocked its own lane: %v %v", pending, err)
	}
	// Unrelated actor keeps its own ordering.
	if pending[0].ID >= pending[1].ID {
		t.Fatalf("lane order broken across actors: %v", pending)
	}
	// The archive fence is the reason the row stays unfinished. Weakening the
	// dependency check must not let a destructive deletion through.
	settled, err := s.CleanupSettled(ctx, v.ID)
	if err != nil || settled {
		t.Fatal("quarantine bypassed archive fence", err)
	}
	var attempts int
	var reason string
	var finished bool
	if err = s.pool.QueryRow(ctx, "SELECT attempts,last_error,finished FROM bvk_updates WHERE id=1").Scan(&attempts, &reason, &finished); err != nil || attempts != 1 || reason == "" || finished {
		t.Fatal("failure not durably retained", err)
	}
	if err = s.ReleaseQuarantine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishUpdate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishUpdate(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishUpdate(ctx, 3); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantinedUpdateIsVisibleAndReleasable(t *testing.T) {
	s := testStore(t)
	seed(t, s)
	ctx := context.Background()
	if _, err := s.SaveUpdate(ctx, 11, json.RawMessage(`{"message":{"from":{"id":77},"chat":{"id":77,"type":"private"},"text":"poison"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryUpdate(ctx, 11, "permanent failure", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	count, err := s.QuarantineCount(ctx)
	if err != nil || count != 1 {
		t.Fatalf("quarantine not counted: %d %v", count, err)
	}
	rows, err := s.QuarantinedUpdates(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("quarantine not listed: %v %v", rows, err)
	}
	// The operator message must name the affected account, not just the row id.
	if rows[0].ID != 11 || rows[0].ActorID != 77 || rows[0].ChatID != 77 || rows[0].LastError == "" {
		t.Fatalf("quarantined row misread: %+v", rows[0])
	}
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts not reported: %+v", rows[0])
	}
	if err = s.ReleaseQuarantine(ctx, 11); err != nil {
		t.Fatal(err)
	}
	if count, err = s.QuarantineCount(ctx); err != nil || count != 0 {
		t.Fatalf("release left quarantine: %d %v", count, err)
	}
	// Released work is selectable again and keeps its attempt history.
	pending, err := s.PendingUpdates(ctx, 100)
	if err != nil || len(pending) != 1 || pending[0].ID != 11 || pending[0].Attempts != 1 {
		t.Fatalf("released update not selectable: %v %v", pending, err)
	}
	// Releasing an unknown or already released row is not silently accepted.
	if err = s.ReleaseQuarantine(ctx, 11); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("double release accepted: %v", err)
	}
	if err = s.FinishUpdate(ctx, 11); err != nil {
		t.Fatal(err)
	}
}

func TestDraftCreationAtomicallyMovesAttachmentAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	if err := s.Transact(ctx, func(tx domain.Tx) error { v.Status = domain.Cancelled; return tx.SaveTicket(ctx, &v) }); err != nil {
		t.Fatal(err)
	}
	d := domain.Draft{Generation: "generation", Step: "confirm", Department: domain.IT, Text: "Description", Attachments: []domain.Message{{TelegramChatID: 1, TelegramMessageID: 14, FileID: "draft-file", FileName: "draft.txt", MediaType: "document", Text: "Caption"}}}
	if err := s.PutDraft(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	svc := tickets.New(s)
	created, err := svc.CreateDraft(ctx, 1, d.Generation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateDraft(ctx, 1, d.Generation)
	if err != nil || created.ID != second.ID {
		t.Fatal("duplicate submit", second, err)
	}
	messages, err := s.Messages(ctx, created.ID)
	if err != nil || len(messages) != 1 || messages[0].FileID != "draft-file" || !messages[0].DeferredDelivery {
		t.Fatal("attachment lost", messages, err)
	}
	stored, err := s.LoadDraft(ctx, 1)
	if err != nil || stored.Step != "submitted" || stored.TicketID != created.ID {
		t.Fatal("draft commit not atomic", stored, err)
	}
	if _, err = svc.CreateDraft(ctx, 1, "old"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old generation accepted", err)
	}
}

func TestLegacyDraftAndConfirmedPartsSurviveMigration(t *testing.T) {
	s := testStore(t)
	seed(t, s)
	ctx := context.Background()
	if err := s.SaveDraft(ctx, 1, domain.IT, "Legacy description"); err != nil {
		t.Fatal(err)
	}
	d, err := s.LoadDraft(ctx, 1)
	if err != nil || d.Text != "Legacy description" {
		t.Fatal(d, err)
	}
	d.Generation = "resumed"
	d.Step = "confirm"
	if err = s.PutDraft(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSendPart(ctx, "send", 0, 123); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := s.SendPart(ctx, "send", 0)
	if err != nil || id != 123 {
		t.Fatal("confirmed part lost", err)
	}
	d, err = s.LoadDraft(ctx, 1)
	if err != nil || d.Generation != "resumed" {
		t.Fatal("form lost after migration", d, err)
	}
}

func TestDraftSubmissionRollbackLeavesNoPartialTicket(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	if err := s.Transact(ctx, func(tx domain.Tx) error {
		v.Status = domain.Cancelled
		if err := tx.SaveTicket(ctx, &v); err != nil {
			return err
		}
		return tx.AddMessage(ctx, &domain.Message{TicketID: v.ID, AuthorID: 1, Direction: "inbound", TelegramChatID: 1, TelegramMessageID: 14})
	}); err != nil {
		t.Fatal(err)
	}
	d := domain.Draft{Generation: "rollback", Step: "confirm", Department: domain.IT, Text: "Problem", Attachments: []domain.Message{{TelegramChatID: 1, TelegramMessageID: 14, FileID: "duplicate-source", MediaType: "document"}}}
	if err := s.PutDraft(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.New(s).CreateDraft(ctx, 1, d.Generation); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expected attachment conflict", err)
	}
	if _, err := s.ActiveTicket(ctx, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("partial ticket created", err)
	}
	d, err := s.LoadDraft(ctx, 1)
	if err != nil || d.Step != "confirm" || d.TicketID != 0 || len(d.Attachments) != 1 {
		t.Fatal("draft consumed on rollback", d, err)
	}
	var operations int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM bvk_outbox").Scan(&operations); err != nil || operations != 0 {
		t.Fatal("partial outbox committed", operations, err)
	}
}

func TestQueueCommandAndPromptFenceTicketMessages(t *testing.T) {
	s := testStore(t)
	v := seed(t, s)
	ctx := context.Background()
	if _, err := s.SaveUpdate(ctx, 1, json.RawMessage(`{"message":{"from":{"id":2},"chat":{"id":-100,"type":"supergroup"},"message_thread_id":1,"text":"/close 1 final"}}`)); err != nil {
		t.Fatal(err)
	}
	// Use the actual generated ticket ID in the command.
	if _, err := s.pool.Exec(ctx, "UPDATE bvk_updates SET payload=jsonb_set(payload,'{message,text}',to_jsonb($1::text)) WHERE id=1", fmt.Sprintf("/close %d final", v.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveUpdate(ctx, 2, json.RawMessage(`{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"dependent client message"}}`)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingUpdates(ctx, 100)
	if err != nil || len(pending) != 1 || pending[0].ID != 1 {
		t.Fatal("queue command did not fence ticket", pending, err)
	}
	if err = s.FinishUpdate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishUpdate(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveWorkflow(ctx, 2, -100, 1, "active_action", []byte(fmt.Sprintf(`{"TicketID":%d,"Action":"close"}`, v.ID))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveUpdate(ctx, 3, json.RawMessage(`{"message":{"from":{"id":2},"chat":{"id":-100,"type":"supergroup"},"message_thread_id":1,"text":"prompt completion"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveUpdate(ctx, 4, json.RawMessage(`{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"dependent client message"}}`)); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingUpdates(ctx, 100)
	if err != nil || len(pending) != 1 || pending[0].ID != 3 {
		t.Fatal("queue prompt did not fence ticket", pending, err)
	}
}
