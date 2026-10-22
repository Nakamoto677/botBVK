package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
)

// Explicit opt-in only: never send messages as part of the ordinary test suite.
// The configured groups must be dedicated/authorized for a live pilot.
func TestLivePrereleaseTelegram(t *testing.T) {
	if os.Getenv("TEST_TELEGRAM_LIVE") != "1" {
		t.Skip("TEST_TELEGRAM_LIVE=1 required")
	}
	path := os.Getenv("TEST_TELEGRAM_CONFIG")
	if path == "" {
		t.Fatal("TEST_TELEGRAM_CONFIG required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ValidateTelegram(); err != nil {
		t.Fatal(err)
	}
	c := NewClient(cfg.Token)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	for _, dept := range []domain.Department{domain.IT, domain.OneC, domain.Installation, domain.Field} {
		t.Run(string(dept), func(t *testing.T) {
			chat := cfg.Departments[dept].ChatID
			topic, err := c.Topic(ctx, chat, "BVK release audit "+time.Now().UTC().Format("20060102-150405"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("created temporary audit topic: chat=%d topic=%d", chat, topic)
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				if e := c.Call(cleanup, "deleteForumTopic", map[string]any{"chat_id": chat, "message_thread_id": topic}, nil); e != nil {
					t.Errorf("cleanup audit topic chat=%d topic=%d: %v", chat, topic, e)
				}
			})
			m, e := c.Send(ctx, chat, topic, "Предрелизная проверка BVK. Временная тестовая тема.", [][]Button{{{Text: "Тестовая ссылка", URL: "https://telegram.org"}}})
			if e != nil {
				t.Fatal(e)
			}
			if e = c.Call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": m.ID, "text": "Предрелизная проверка: редактирование работает."}, nil); e != nil {
				t.Fatal(e)
			}
			if e = c.HideMessageKeyboard(ctx, chat, m.ID); e != nil && messageEditError(e) != nil {
				t.Fatal(e)
			}
			if dept == domain.IT {
				file := filepath.Join(t.TempDir(), "bvk-audit.txt")
				if e = os.WriteFile(file, []byte("BVK pre-release synthetic attachment. No customer data.\n"), 0600); e != nil {
					t.Fatal(e)
				}
				if !pause(ctx, 3200*time.Millisecond) {
					t.Fatal(ctx.Err())
				}
				if e = c.SendLocalFile(ctx, chat, topic, file, "bvk-audit.txt"); e != nil {
					t.Fatal(e)
				}
			}
			if e = c.CloseTopic(ctx, chat, topic); e != nil {
				t.Fatal(e)
			}
			if e = c.ReopenTopic(ctx, chat, topic); e != nil {
				t.Fatal(e)
			}
			t.Log(fmt.Sprintf("%s: send, edit, hide keyboard, close, reopen verified", dept))
		})
	}
}
