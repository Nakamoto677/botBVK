package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"testing"
)

// A stale screen must answer its callback exactly once, with the explanation the
// user needs, and must not be replaced afterwards by the generic error answer.
// Before the fix the rejected draft returned nil, so the update was treated as
// successful and the callback was never answered at all: the spinner hung.
func TestStaleDraftAnswersCallbackOnceWithExplanation(t *testing.T) {
	a, s, _, _, requests := clientUIFixture(t)
	s.workflows = map[promptKey][]byte{}
	// A draft that is present but carries no generation, so any tapped screen is
	// outdated. The generation is what binds a screen to a form.
	s.workflows[promptKey{1, 1, 0}] = nil
	if err := a.saveDraft(context.Background(), 1, draft{Department: domain.IT, Text: "saved"}); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(Update{Callback: &Callback{
		ID:      "cb-1",
		From:    Person{ID: 1, FirstName: "Клиент"},
		Message: &Message{ID: 80, Chat: Chat{ID: 1, Type: "private"}},
		Data:    "submit:old-generation",
	}})
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := a.getDraft(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	entry := domain.StoredUpdate{ID: 1, Payload: raw}
	if err := a.processUpdate(context.Background(), entry); err != nil {
		t.Fatalf("stale screen must be handled as a business conflict: %v", err)
	}

	answers := []string{}
	for _, req := range *requests {
		if _, isAnswer := req["callback_query_id"]; !isAnswer {
			continue
		}
		text, _ := req["text"].(string)
		answers = append(answers, text)
	}
	if len(answers) != 1 {
		t.Fatalf("callback answered %d times, want exactly 1: %v", len(answers), answers)
	}
	if answers[0] != "Этот экран больше не актуален. Откройте текущее состояние: /form или /current." {
		t.Fatalf("callback answer does not explain the outdated screen: %q", answers[0])
	}
	// The rejected screen must not advance the form it no longer belongs to.
	after, _, err := a.getDraft(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Step != before.Step || after.Generation != before.Generation || after.Text != before.Text {
		t.Fatalf("stale screen changed the form: before=%+v after=%+v", before, after)
	}
}
