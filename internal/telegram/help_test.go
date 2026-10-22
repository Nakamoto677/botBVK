package telegram

import (
	"context"
	"strings"
	"testing"
)

// The client's Помощь button has to produce a readable answer that stays on screen.
// It used to be registered under the disposable home scenario, so the next cleanup
// deleted it and the button looked dead. This drives the real tap path: the reply
// keyboard sends the label as ordinary text, which the adapter maps to /help.
func TestClientHelpButtonAnswersAndKeepsTheAnswer(t *testing.T) {
	adapter, store, _, m, requests := clientUIFixture(t)
	m.Text = "Помощь"
	if err := adapter.Handle(context.Background(), Update{Message: m}); err != nil {
		t.Fatal(err)
	}

	// Exactly one message must reach the client, and it must be the help text.
	delivered := []string{}
	for _, req := range *requests {
		if text, ok := req["text"].(string); ok {
			delivered = append(delivered, text)
		}
	}
	if len(delivered) != 1 {
		t.Fatalf("help produced %d messages, want one: %v", len(delivered), delivered)
	}
	if !strings.Contains(delivered[0], "Создать заявку") || !strings.Contains(delivered[0], "Мои заявки") {
		t.Fatalf("help answer does not describe the client flow: %q", delivered[0])
	}
	// The answer must carry the client menu, otherwise the person is left without a way on.
	assertReplyLabels(t, (*requests)[len(*requests)-1], "Создать заявку", "Мои заявки", "Помощь")

	// And it must be retained, not queued for deletion.
	kept := store.aux[auxKey{1, 1, 0, "client_kept"}]
	if len(kept) != 1 {
		t.Fatalf("help answer was not retained: %v", store.aux)
	}
	disposable := store.aux[auxKey{1, 1, 0, "client_home"}]
	if len(disposable) != 0 {
		t.Fatalf("help answer stayed disposable and would be deleted: %v", disposable)
	}
	// Nothing may be queued to delete the answer either.
	for _, op := range store.operations {
		if op.Kind == "delete_aux_message" {
			t.Fatalf("help answer queued for deletion: %v", op)
		}
	}
}

// A transient flow screen must stay disposable, otherwise cleanup would stop
// removing the intermediate steps it exists for.
func TestTransientScreensStayDisposable(t *testing.T) {
	m := &Message{Chat: Chat{ID: 1, Type: "private"}}
	if got := clientAuxScenario(m, "draft", "Выберите направление."); got != "client_kept" {
		t.Fatalf("a known milestone was not retained: %q", got)
	}
	if got := clientAuxScenario(m, "draft", "Временный промежуточный экран"); got != "draft" {
		t.Fatalf("transient screen was wrongly retained: %q", got)
	}
	group := &Message{Chat: Chat{ID: -100, Type: "supergroup"}}
	if got := clientAuxScenario(group, "draft", clientHelpText); got != "draft" {
		t.Fatalf("group scenario was rewritten: %q", got)
	}
}
