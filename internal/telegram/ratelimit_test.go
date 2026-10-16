package telegram

import (
	"context"
	"testing"
	"time"
)

// Only a method that puts a new message into the chat may consume the per-chat
// message budget. Charging it for edits and topic lifecycle calls is what made one
// ticket claim wait roughly fourteen seconds before anything reached the client.
func TestOnlyNewMessagesConsumeTheChatMessageInterval(t *testing.T) {
	worker := NewWorker(nil, nil, Config{})
	worker.interval = 3 * time.Second
	ctx := context.Background()

	if err := worker.rate(ctx, -100, "sendMessage"); err != nil {
		t.Fatal(err)
	}
	// A second new message has to wait for the chat slot.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	if err := worker.rate(short, -100, "sendMessage"); err == nil {
		t.Fatal("second new message did not wait for the chat slot")
	}
	cancel()
	// Editing a card, creating a topic and closing one must not.
	for _, method := range []string{"editMessageText", "editMessageReplyMarkup", "createForumTopic", "closeForumTopic", "reopenForumTopic", "deleteForumTopic", "deleteMessage"} {
		quick, stop := context.WithTimeout(ctx, 300*time.Millisecond)
		err := worker.rate(quick, -100, method)
		stop()
		if err != nil {
			t.Fatalf("%s was charged the message interval: %v", method, err)
		}
	}
}

func TestFileUploadsCountAsNewMessages(t *testing.T) {
	worker := NewWorker(nil, nil, Config{})
	worker.interval = 3 * time.Second
	ctx := context.Background()
	if err := worker.rate(ctx, -100, "sendDocument"); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := worker.rate(short, -100, "sendPhoto"); err == nil {
		t.Fatal("a second file was not charged the chat slot")
	}
}

// The global spacing survives for everything, so a burst of edits cannot escape
// the overall cap even though it no longer pays the per-chat message interval.
func TestNonMessageCallsStillRespectGlobalSpacing(t *testing.T) {
	worker := NewWorker(nil, nil, Config{})
	worker.interval = time.Second
	ctx := context.Background()
	started := time.Now()
	for i := 0; i < 5; i++ {
		if err := worker.rate(ctx, -100, "editMessageText"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("five edits escaped global spacing in %v", elapsed)
	}
}
