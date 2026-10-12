package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"bot-bvk/internal/archive"
	"bot-bvk/internal/domain"
)

type completionTarget struct {
	ChatID            int64  `json:"chat_id"`
	TopicID           int64  `json:"topic_id"`
	RoutingGeneration int64  `json:"routing_generation"`
	CompletionKey     string `json:"completion_key"`
	BackupDirectory   string `json:"backup_directory,omitempty"`
}
type cleanupSettler interface {
	CleanupSettled(context.Context, int64) (bool, error)
}
type cleanupRetry interface {
	CleanupRetry(context.Context, int64) error
}
type cleanupProblem interface {
	CleanupProblem(context.Context, int64) (string, error)
}

// RunCleanup isolates dump/restore latency from ordinary reminder ticks.
func (s *Scheduler) RunCleanup(ctx context.Context) error {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		if err := s.CleanupTick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("completion cleanup failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (s *Scheduler) CleanupTick(ctx context.Context) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	all, err := s.Store.Tickets(ctx)
	if err != nil {
		return err
	}
	for _, ticket := range all {
		events, err := s.Store.Events(ctx, ticket.ID)
		if err != nil {
			return err
		}
		for _, completion := range events {
			var target completionTarget
			if completion.Kind != "completion_cleanup_requested" || json.Unmarshal(completion.Data, &target) != nil || target.ChatID == 0 || target.TopicID == 0 || target.CompletionKey == "" {
				continue
			}
			if cleanupQueued(events, target.CompletionKey) {
				continue
			}
			if err := s.prepareCleanup(ctx, ticket, events, completion, target); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				if err = s.cleanupBlocked(ctx, ticket, events, target.CompletionKey, err.Error()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (s *Scheduler) prepareCleanup(ctx context.Context, t domain.Ticket, events []domain.Event, completion domain.Event, target completionTarget) error {
	if t.Status.Active() && t.TopicChatID == target.ChatID && t.TopicID == target.TopicID {
		return fmt.Errorf("completed topic is still an active binding")
	}
	if retry, ok := s.Store.(cleanupRetry); ok {
		if err := retry.CleanupRetry(ctx, t.ID); err != nil {
			return err
		}
	}
	settled, ok := s.Store.(cleanupSettler)
	if !ok {
		return fmt.Errorf("delivery/archive settlement check is unavailable")
	}
	ready, err := settled.CleanupSettled(ctx, t.ID)
	if err != nil {
		return err
	}
	if !ready {
		if problems, ok := s.Store.(cleanupProblem); ok {
			cause, err := problems.CleanupProblem(ctx, t.ID)
			if err != nil {
				return err
			}
			if cause != "" {
				return fmt.Errorf("accepted operation not settled: %s", cause)
			}
		}
		return fmt.Errorf("accepted message deliveries or attachments are still pending or failed")
	}
	for _, event := range events {
		if event.Kind == "media_edit_unsupported" && !event.CreatedAt.After(completion.CreatedAt) {
			return fmt.Errorf("message media was edited without a preserved archive; administrator must restore the missing original attachment")
		}
	}
	msgs, err := s.Store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	dir := proofDirectory(events, target.CompletionKey)
	manifest, loadErr := archive.Load(dir)
	if loadErr != nil || !manifest.CoversCompletion(t, msgs, events, completion) {
		if err := archive.CheckMessages(msgs); err != nil {
			return err
		}
		if s.Backup == nil {
			return fmt.Errorf("automatic backup is not configured")
		}
		dir, err = s.Backup(ctx)
		if err != nil {
			return fmt.Errorf("backup creation/restore verification failed: %w", err)
		}
		manifest, err = archive.Load(dir)
		if err != nil {
			return fmt.Errorf("backup verification manifest unavailable: %w", err)
		}
		if !manifest.CoversCompletion(t, msgs, events, completion) {
			return fmt.Errorf("verified backup does not cover saved completion and transcript")
		}
	}
	return s.Store.Transact(ctx, func(tx domain.Tx) error {
		latest, err := tx.GetTicket(ctx, t.ID)
		if err != nil {
			return err
		}
		if latest.Version != t.Version {
			return domain.ErrConflict
		}
		target.BackupDirectory = dir
		payload, _ := json.Marshal(target)
		if err = tx.Enqueue(ctx, domain.Operation{Kind: "delete_topic", TicketID: t.ID, Payload: payload}); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"key": target.CompletionKey, "completion_key": target.CompletionKey, "backup_directory": dir, "chat_id": target.ChatID, "topic_id": target.TopicID, "routing_generation": target.RoutingGeneration})
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_queued", Data: data, CreatedAt: s.cleanupNow()})
	})
}
func (s *Scheduler) cleanupNow() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
func cleanupQueued(events []domain.Event, key string) bool {
	queued := false
	for _, event := range events {
		var data struct {
			Key           string `json:"key"`
			CompletionKey string `json:"completion_key"`
		}
		if json.Unmarshal(event.Data, &data) != nil || (data.Key != key && data.CompletionKey != key) {
			continue
		}
		switch event.Kind {
		case "cleanup_queued", "topic_deleted":
			queued = true
		case "cleanup_deferred":
			queued = false
		}
	}
	return queued
}
func proofDirectory(events []domain.Event, key string) string {
	dir := ""
	for _, event := range events {
		if event.Kind != "cleanup_queued" {
			continue
		}
		var data struct {
			Key           string `json:"key"`
			CompletionKey string `json:"completion_key"`
			Directory     string `json:"backup_directory"`
		}
		if json.Unmarshal(event.Data, &data) == nil && (data.Key == key || data.CompletionKey == key) {
			dir = data.Directory
		}
	}
	return dir
}
func (s *Scheduler) cleanupBlocked(ctx context.Context, t domain.Ticket, events []domain.Event, key, cause string) error {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind != "cleanup_blocked" {
			continue
		}
		var data struct {
			Key   string `json:"key"`
			Cause string `json:"cause"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.Key == key {
			if data.Cause == cause {
				return nil
			}
			break
		}
	}
	return s.Store.Transact(ctx, func(tx domain.Tx) error {
		if _, err := tx.GetTicket(ctx, t.ID); err != nil {
			return err
		}
		admins := map[int64]bool{}
		if s.Config.AdminID != 0 {
			admins[s.Config.AdminID] = true
		}
		for _, staff := range s.Config.Staff {
			if staff.Role == "admin" {
				admins[staff.ID] = true
			}
		}
		for id := range admins {
			payload, _ := json.Marshal(map[string]any{"user_id": id, "text": fmt.Sprintf("Заявка #%d: удаление завершённого топика отложено. Причина: %s. История сохранена; бот повторит попытку автоматически после устранения причины.", t.ID, cause)})
			if err := tx.Enqueue(ctx, domain.Operation{Kind: "access_notification", Payload: payload}); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]string{"key": key, "cause": cause})
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_blocked", Data: data, CreatedAt: s.cleanupNow()})
	})
}
