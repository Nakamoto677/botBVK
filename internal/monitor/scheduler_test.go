package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bot-bvk/internal/archive"
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

type fakeStore struct {
	domain.Store
	ticket     domain.Ticket
	events     []domain.Event
	operations []domain.Operation
}

func (f *fakeStore) Tickets(context.Context) ([]domain.Ticket, error) {
	return []domain.Ticket{f.ticket}, nil
}
func (f *fakeStore) Messages(context.Context, int64) ([]domain.Message, error) { return nil, nil }
func (f *fakeStore) Events(context.Context, int64) ([]domain.Event, error)     { return f.events, nil }
func (f *fakeStore) Transact(ctx context.Context, fn func(domain.Tx) error) error {
	return fn(&fakeTx{store: f})
}

type fakeTx struct {
	domain.Tx
	store *fakeStore
}

func (f *fakeTx) GetTicket(context.Context, int64) (domain.Ticket, error) { return f.store.ticket, nil }
func (f *fakeTx) AddEvent(_ context.Context, e domain.Event) error {
	f.store.events = append(f.store.events, e)
	return nil
}
func (f *fakeTx) Enqueue(_ context.Context, o domain.Operation) error {
	f.store.operations = append(f.store.operations, o)
	return nil
}
func TestFirstResponseUsesWorkingClockAndDeduplicates(t *testing.T) {
	calendar := tickets.DefaultCalendar()
	loc := calendar.Location
	// Friday 17:50 -> Monday 09:20 is exactly 30 working minutes.
	created := time.Date(2026, 10, 9, 17, 50, 0, 0, loc)
	store := &fakeStore{ticket: domain.Ticket{ID: 1, Version: 1, Status: domain.New, CreatedAt: created}}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, loc)
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: t.TempDir(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 2, StartHour: 9, EndHour: 18, TopicRetentionDays: 30}, Calendar: calendar, Now: func() time.Time { return now }}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 0 {
		t.Fatal("weekend advanced ordinary deadline")
	}
	now = time.Date(2026, 10, 12, 9, 20, 0, 0, loc)
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.operations) != 1 || store.operations[0].Kind != "team_notification" {
		t.Fatalf("expected one reminder, got %+v", store.operations)
	}
}
func TestCleanupProofRejectsLateEditsAndCanBeRequeued(t *testing.T) {
	now := time.Now().UTC()
	closed := now.Add(-31 * 24 * time.Hour)
	store := &fakeStore{ticket: domain.Ticket{ID: 1, Version: 1, Status: domain.Closed, ClosedAt: &closed, TopicChatID: -1001, TopicID: 10}}
	backupDir := t.TempDir()
	dir := filepath.Join(backupDir, "snapshot")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dump := []byte("verified dump fixture")
	hash := sha256.Sum256(dump)
	if err := os.WriteFile(filepath.Join(dir, "database.dump"), dump, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := archive.Manifest{CreatedAt: now, Verified: true, DumpSHA256: hex.EncodeToString(hash[:]), Tickets: map[int64]archive.TicketSnapshot{1: {Version: 1, EventID: 1, Complete: true}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: backupDir, TopicRetentionDays: 30}, Calendar: tickets.DefaultCalendar(), Now: func() time.Time { return now }}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 1 || store.operations[0].Kind != "delete_topic" {
		t.Fatal("eligible verified topic not scheduled")
	}
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 10); err != nil {
		t.Fatal(err)
	}
	store.events = append(store.events, domain.Event{ID: 2, Kind: "message_edited"})
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 10); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("late edit failed to invalidate deletion: %v", err)
	}
	key := "cleanup:-1001:10"
	data, _ := json.Marshal(map[string]string{"key": key})
	store.events = append(store.events, domain.Event{Kind: "cleanup_deferred", Data: data})
	if hasKey(store.events, key) {
		t.Fatal("deferred cleanup permanently suppressed fresh backup retry")
	}
}
