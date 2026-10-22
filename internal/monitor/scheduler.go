package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
	for _, t := range all {
		events, err := s.Store.Events(ctx, t.ID)
		if err != nil {
			return err
		}
		if t.Status.Active() && s.Calendar.IsWorking(now) {
			if t.Urgency == domain.Urgent && t.Status == domain.New && t.AssigneeID == 0 && s.Calendar.IsWorking(now) {
				baseline := t.CreatedAt
				for _, ev := range events {
					if (ev.Kind == "transferred" || ev.Kind == "reopened") && ev.CreatedAt.After(baseline) {
						baseline = ev.CreatedAt
					}
				}
				for _, minutes := range []int{10, 20} {
					if s.Calendar.WorkDuration(baseline, now) < time.Duration(minutes)*time.Minute {
						continue
					}
					message := fmt.Sprintf("🔴 Срочная заявка #%d ожидает назначения уже 10 рабочих минут. Возьмите заявку в работу.", t.ID)
					if minutes == 20 {
						message = fmt.Sprintf("🔴 Срочная заявка #%d всё ещё без исполнителя. С момента поступления прошло 20 рабочих минут.", t.ID)
					}
					if err = s.once(ctx, t, events, fmt.Sprintf("urgent_assignment_%d", minutes), baseline, "urgent_assignment_reminder", map[string]string{"text": message, "baseline": baseline.UTC().Format(time.RFC3339Nano)}); err != nil {
						return err
					}
				}
			}
			if t.FirstResponseAt == nil && s.Calendar.WorkDuration(t.CreatedAt, now) >= time.Duration(s.Config.FirstResponseMinutes)*time.Minute {
				message := fmt.Sprintf("Заявка #%d: нет первого ответа поддержки за %d рабочих минут.", t.ID, s.Config.FirstResponseMinutes)
				if t.Urgency == domain.Urgent {
					message = fmt.Sprintf("🔴 Срочная заявка #%d: клиент не получил первого ответа за %d рабочих минут.", t.ID, s.Config.FirstResponseMinutes)
				}
				kind := "team_notification"
				if t.Urgency == domain.Urgent {
					kind = "first_response_reminder"
				}
				if err = s.remind(ctx, t, events, "first_response", t.CreatedAt, kind, message); err != nil {
					return err
				}
			}
			if t.Status == domain.Waiting && !t.IsGroup() {
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
	if sweeper, ok := s.Store.(interface {
		RetireFinishedFailures(context.Context) (int, error)
	}); ok {
		// A failed operation is terminal and the bot never retries it, while /ready
		// reports not-ready for as long as one exists. Clearing what cannot be retried
		// keeps readiness meaningful; failures on live tickets are kept by the store so
		// that signal stays visible.
		if retired, err := sweeper.RetireFinishedFailures(ctx); err != nil {
			slog.Error("failed to retire finished failures", "error", err)
		} else if retired > 0 {
			slog.Info("retired failed operations that cannot be retried", "count", retired)
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
