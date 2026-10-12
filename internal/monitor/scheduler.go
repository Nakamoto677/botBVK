package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"bot-bvk/internal/archive"
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

type Scheduler struct {
	Store     domain.Store
	Config    config.Config
	Calendar  tickets.Calendar
	Now       func() time.Time
	Backup    func(context.Context) (string, error)
	cleanupMu sync.Mutex
}

func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("monitor tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Scheduler) Tick(ctx context.Context) error {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	all, err := s.Store.Tickets(ctx)
	if err != nil {
		return err
	}
	for _, t := range all {
		events, err := s.Store.Events(ctx, t.ID)
		if err != nil {
			return err
		}
		msgs, err := s.Store.Messages(ctx, t.ID)
		if err != nil {
			return err
		}
		if t.Status.Active() {
			if t.FirstResponseAt == nil && s.Calendar.WorkDuration(t.CreatedAt, now) >= time.Duration(s.Config.FirstResponseMinutes)*time.Minute {
				if err = s.remind(ctx, t, events, "first_response", t.CreatedAt, "team_notification", fmt.Sprintf("Заявка #%d: нет первого ответа поддержки за %d рабочих минут.", t.ID, s.Config.FirstResponseMinutes)); err != nil {
					return err
				}
			}
			if t.Status == domain.InProgress {
				baseline := t.CreatedAt
				for _, m := range msgs {
					if (m.Direction == "outbound" || m.Direction == "internal") && m.CreatedAt.After(baseline) {
						baseline = m.CreatedAt
					}
				}
				for _, ev := range events {
					if (ev.Kind == "claimed" || ev.Kind == "reassigned" || ev.Kind == "reopened" || ev.Kind == "transferred" || ev.Kind == "status_changed") && ev.CreatedAt.After(baseline) {
						baseline = ev.CreatedAt
					}
				}
				if s.Calendar.WorkDuration(baseline, now) >= time.Duration(s.Config.IdleHours)*time.Hour {
					if err = s.remind(ctx, t, events, "idle", baseline, "team_notification", fmt.Sprintf("Заявка #%d: нет активности инженера %d рабочих часа. Проверьте статус и следующий шаг.", t.ID, s.Config.IdleHours)); err != nil {
						return err
					}
				}
			}
			if t.Status == domain.Waiting {
				baseline := t.CreatedAt
				for _, ev := range events {
					if ev.Kind == "status_changed" && ev.CreatedAt.After(baseline) {
						baseline = ev.CreatedAt
					}
				}
				hours := s.Config.EndHour - s.Config.StartHour
				if s.Calendar.WorkDuration(baseline, now) >= time.Duration(s.Config.WaitingDays*hours)*time.Hour {
					if err = s.remind(ctx, t, events, "waiting", baseline, "notify_client", fmt.Sprintf("По заявке #%d ожидаем ваш ответ. Напишите, пожалуйста, удалось ли проверить решение.", t.ID)); err != nil {
						return err
					}
				}
			}
		}

	}
	return nil
}
func hasKey(events []domain.Event, key string) bool {
	found := false
	for _, e := range events {
		var p struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(e.Data, &p) == nil && p.Key == key {
			found = e.Kind != "cleanup_deferred"
		}
	}
	return found
}
func (s *Scheduler) remind(ctx context.Context, t domain.Ticket, events []domain.Event, label string, baseline time.Time, kind, text string) error {
	return s.once(ctx, t, events, label, baseline, kind, map[string]string{"text": text})
}
func (s *Scheduler) once(ctx context.Context, t domain.Ticket, events []domain.Event, label string, baseline time.Time, kind string, payload any) error {
	key := label + ":" + baseline.UTC().Format(time.RFC3339Nano)
	if hasKey(events, key) {
		return nil
	}
	return s.Store.Transact(ctx, func(tx domain.Tx) error {
		latest, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if latest.Version != t.Version || !latest.Status.Active() {
			return nil
		}
		b, _ := json.Marshal(payload)
		if e = tx.Enqueue(ctx, domain.Operation{Kind: kind, TicketID: t.ID, Payload: b}); e != nil {
			return e
		}
		data, _ := json.Marshal(map[string]string{"key": key})
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "reminder", Data: data})
	})
}

// BeforeDelete validates only a new explicit completion and its durable proof.
// It is called while the worker holds the ticket lock to fence concurrent edits.
func (s *Scheduler) BeforeDelete(ctx context.Context, tx domain.Tx, t domain.Ticket, chatID, topicID int64) error {
	if fence, ok := s.Store.(interface {
		CleanupFence(context.Context, domain.Tx, int64) error
	}); ok {
		if err := fence.CleanupFence(ctx, tx, t.ID); err != nil {
			return err
		}
	}
	events, err := s.Store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	msgs, err := s.Store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	if t.Status.Active() && t.TopicChatID == chatID && t.TopicID == topicID {
		return domain.ErrConflict
	}
	for _, event := range events {
		var target completionTarget
		if event.Kind != "completion_cleanup_requested" || json.Unmarshal(event.Data, &target) != nil || target.ChatID != chatID || target.TopicID != topicID || target.CompletionKey == "" {
			continue
		}
		dir := proofDirectory(events, target.CompletionKey)
		if dir == "" {
			return domain.ErrConflict
		}
		manifest, err := archive.Load(dir)
		if err != nil || !manifest.CoversCompletion(t, msgs, events, event) {
			return domain.ErrConflict
		}
		return nil
	}
	return domain.ErrConflict
}
