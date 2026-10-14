package telegram

import (
	"context"
	"sync"
	"time"
)

// Telegram recommends at most one message per second to a private chat,
// twenty per minute to a group, and about thirty per second bot-wide.
// Reservations are shared by all workers through their Client.
type rateLimiter struct {
	mu       sync.Mutex
	nextChat map[int64]time.Time
	nextBot  time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{nextChat: make(map[int64]time.Time)}
}

func (l *rateLimiter) wait(ctx context.Context, chat int64, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	l.mu.Lock()
	when := time.Now()
	if next := l.nextChat[chat]; next.After(when) {
		when = next
	}
	if l.nextBot.After(when) {
		when = l.nextBot
	}
	l.nextChat[chat] = when.Add(interval)
	l.nextBot = when.Add(35 * time.Millisecond)
	l.mu.Unlock()
	if delay := time.Until(when); delay > 0 && !pause(ctx, delay) {
		return ctx.Err()
	}
	return nil
}

func (l *rateLimiter) coolDown(chat int64, delay time.Duration) {
	if chat == 0 || delay <= 0 {
		return
	}
	l.mu.Lock()
	until := time.Now().Add(delay)
	if until.After(l.nextChat[chat]) {
		l.nextChat[chat] = until
	}
	l.mu.Unlock()
}
