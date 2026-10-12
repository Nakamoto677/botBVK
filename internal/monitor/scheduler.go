package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"bot-bvk/internal/archive"
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

type Scheduler struct {
	Store    domain.Store
	Config   config.Config
	Calendar tickets.Calendar
	Now      func() time.Time
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
	backup, _ := archive.Latest(s.Config.BackupDir)
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
				if err = s.remind(ctx, t, events, "first_response", t.CreatedAt, "dispatcher_message", fmt.Sprintf("Заявка #%d: нет первого ответа поддержки за %d рабочих минут.", t.ID, s.Config.FirstResponseMinutes)); err != nil {
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
			if t.Emergency {
				baseline := t.CreatedAt
				for _, ev := range events {
					if (ev.Kind == "emergency" || ev.Kind == "transferred" || ev.Kind == "reopened") && ev.CreatedAt.After(baseline) {
						baseline = ev.CreatedAt
					}
				}
				team := s.Config.Departments[t.Department]
				kind := "access_notification"
				payload := map[string]any{"user_id": team.DutyUserID, "text": fmt.Sprintf("Аварийная заявка #%d в отделе %s. Откройте очередь и возьмите в работу.", t.ID, t.Department)}
				if team.DutyUserID == 0 {
					kind = "dispatcher_message"
					payload = map[string]any{"text": fmt.Sprintf("Авария #%d: для отдела %s не настроен дежурный! Назначьте инженера вручную.", t.ID, t.Department)}
				}
				label := fmt.Sprintf("emergency:%s:%d", t.Department, team.DutyUserID)
				if err = s.once(ctx, t, events, label, baseline, kind, payload); err != nil {
					return err
				}
			}
		}
		// Current closed topic and former transferred topics share the same archive.
		// Any unarchived attachment or unverified backup prevents deletion.
		if !backup.Covers(t, msgs, events) {
			continue
		}
		type candidate struct {
			ChatID  int64     `json:"chat_id"`
			TopicID int64     `json:"topic_id"`
			At      time.Time `json:"-"`
		}
		var targets []candidate
		if (t.Status == domain.Closed || t.Status == domain.Cancelled) && t.ClosedAt != nil && t.TopicID != 0 {
			targets = append(targets, candidate{t.TopicChatID, t.TopicID, *t.ClosedAt})
		}
		for _, ev := range events {
			if ev.Kind == "topic_retired" {
				var c candidate
				if json.Unmarshal(ev.Data, &c) == nil && c.ChatID != 0 && c.TopicID != 0 {
					c.At = ev.CreatedAt
					targets = append(targets, c)
				}
			}
		}
		for _, target := range targets {
			if now.Sub(target.At) < time.Duration(s.Config.TopicRetentionDays)*24*time.Hour {
				continue
			}
			key := fmt.Sprintf("cleanup:%d:%d", target.ChatID, target.TopicID)
			if hasKey(events, key) {
				continue
			}
			if err = s.Store.Transact(ctx, func(tx domain.Tx) error {
				latest, e := tx.GetTicket(ctx, t.ID)
				if e != nil {
					return e
				}
				if latest.Version != t.Version {
					return domain.ErrConflict
				}
				if latest.TopicID == target.TopicID && latest.TopicChatID == target.ChatID && latest.Status.Active() {
					return domain.ErrConflict
				}
				b, _ := json.Marshal(target)
				if e = tx.Enqueue(ctx, domain.Operation{Kind: "delete_topic", TicketID: t.ID, Payload: b}); e != nil {
					return e
				}
				data, _ := json.Marshal(map[string]string{"key": key})
				return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_queued", Data: data, CreatedAt: now})
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
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

// BeforeDelete is invoked by the outbox worker while it holds the ticket row
// lock. Re-checking the actual files and snapshot here fences late message edits
// and backup corruption between scheduling and execution.
func (s *Scheduler) BeforeDelete(ctx context.Context, _ domain.Tx, t domain.Ticket, chatID, topicID int64) error {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	messages, err := s.Store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	events, err := s.Store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	manifest, err := archive.Latest(s.Config.BackupDir)
	if err != nil || !manifest.Covers(t, messages, events) {
		return domain.ErrConflict
	}
	var retiredAt time.Time
	if t.TopicChatID == chatID && t.TopicID == topicID {
		if t.Status.Active() || t.ClosedAt == nil {
			return domain.ErrConflict
		}
		retiredAt = *t.ClosedAt
	} else {
		for _, event := range events {
			if event.Kind != "topic_retired" {
				continue
			}
			var p struct {
				ChatID  int64 `json:"chat_id"`
				TopicID int64 `json:"topic_id"`
			}
			if json.Unmarshal(event.Data, &p) == nil && p.ChatID == chatID && p.TopicID == topicID {
				retiredAt = event.CreatedAt
			}
		}
	}
	if retiredAt.IsZero() || now.Sub(retiredAt) < time.Duration(s.Config.TopicRetentionDays)*24*time.Hour {
		return domain.ErrConflict
	}
	return nil
}
