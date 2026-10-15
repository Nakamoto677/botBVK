package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	messages   []domain.Message
	settled    bool
}

func (f *fakeStore) Tickets(context.Context) ([]domain.Ticket, error) {
	return []domain.Ticket{f.ticket}, nil
}
func (f *fakeStore) Messages(context.Context, int64) ([]domain.Message, error) {
	return f.messages, nil
}
func (f *fakeStore) CleanupSettled(context.Context, int64) (bool, error)   { return f.settled, nil }
func (f *fakeStore) Events(context.Context, int64) ([]domain.Event, error) { return f.events, nil }
func (f *fakeStore) Transact(ctx context.Context, fn func(domain.Tx) error) error {
	return fn(&fakeTx{store: f})
}

type fakeTx struct {
	domain.Tx
	store *fakeStore
}

func (f *fakeTx) GetTicket(context.Context, int64) (domain.Ticket, error) { return f.store.ticket, nil }
func (f *fakeTx) AddEvent(_ context.Context, e domain.Event) error {
	if e.ID == 0 {
		for _, old := range f.store.events {
			if old.ID >= e.ID {
				e.ID = old.ID + 1
			}
		}
	}
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
func completionFixture(t *testing.T) (*Scheduler, *fakeStore, *int) {
	t.Helper()
	now := time.Now().UTC()
	key := "completion:1:3:-1001:10"
	data, _ := json.Marshal(map[string]any{"chat_id": -1001, "topic_id": 10, "routing_generation": 3, "completion_key": key})
	store := &fakeStore{settled: true, ticket: domain.Ticket{ID: 1, Version: 1, Status: domain.Closed, ClosedAt: &now, TopicChatID: -1001, TopicID: 10}, events: []domain.Event{{ID: 1, Kind: "closed", CreatedAt: now}, {ID: 2, Kind: "completion_cleanup_requested", Data: data, CreatedAt: now}}}
	s := &Scheduler{Store: store, Config: config.Config{AdminID: 99, BackupDir: t.TempDir()}, Calendar: tickets.DefaultCalendar(), Now: func() time.Time { return now }}
	calls := 0
	s.Backup = func(context.Context) (string, error) {
		calls++
		dir := filepath.Join(s.Config.BackupDir, fmt.Sprintf("snapshot%d", calls))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
		dump := []byte("verified restored dump fixture")
		if err := os.WriteFile(filepath.Join(dir, "database.dump"), dump, 0600); err != nil {
			return "", err
		}
		hash := sha256.Sum256(dump)
		snap := archive.TicketSnapshot{Version: store.ticket.Version, Complete: true, MessageHashes: map[int64]string{}}
		for _, m := range store.messages {
			raw, _ := json.Marshal(m)
			mh := sha256.Sum256(raw)
			snap.MessageHashes[m.ID] = hex.EncodeToString(mh[:])
			if m.ID > snap.MessageID {
				snap.MessageID = m.ID
			}
		}
		for _, ev := range store.events {
			if ev.ID > snap.EventID {
				snap.EventID = ev.ID
			}
		}
		manifest := archive.Manifest{CreatedAt: now, Verified: true, DumpSHA256: hex.EncodeToString(hash[:]), Tickets: map[int64]archive.TicketSnapshot{1: snap}}
		raw, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
			return "", err
		}
		return dir, nil
	}
	return s, store, &calls
}
func TestCompletionCleanupImmediatelyAndDetachedAfterReopen(t *testing.T) {
	s, store, calls := completionFixture(t)
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(store.operations) != 1 || store.operations[0].Kind != "delete_topic" {
		t.Fatalf("completion not queued immediately: %d %#v", *calls, store.operations)
	}
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 10); err != nil {
		t.Fatal(err)
	}
	// A new cycle changes routing/version but doesn't invalidate the saved old transcript.
	store.ticket.Version++
	store.ticket.Status = domain.InProgress
	store.ticket.TopicID = 20
	store.ticket.RoutingGeneration = 4
	store.events = append(store.events, domain.Event{ID: 4, Kind: "reopened", CreatedAt: s.cleanupNow().Add(time.Second)})
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 10); err != nil {
		t.Fatalf("detached saved target rejected: %v", err)
	}
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 20); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("new active topic permitted: %v", err)
	}
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(store.operations) != 1 {
		t.Fatal("completion duplicated after restart/tick")
	}
}
func TestHistoricalClosureNeverSwept(t *testing.T) {
	s, store, calls := completionFixture(t)
	store.events = store.events[:1]
	old := s.cleanupNow().Add(-100 * 24 * time.Hour)
	store.ticket.ClosedAt = &old
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || len(store.operations) != 0 {
		t.Fatal("historical closure was bulk cleaned")
	}
}
func TestCompletionWaitsForAcceptedOpsThenAutomaticallyRecovers(t *testing.T) {
	s, store, calls := completionFixture(t)
	store.settled = false
	for range 2 {
		if err := s.CleanupTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 0 || len(store.operations) != 1 || store.operations[0].Kind != "access_notification" {
		t.Fatalf("pending archive not explained/deduplicated: %#v", store.operations)
	}
	store.settled = true
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(store.operations) != 2 || store.operations[1].Kind != "delete_topic" {
		t.Fatal("completion didn't recover automatically")
	}
}
func TestCompletionBackupFailureRetriesAndOldProofRejectsEdits(t *testing.T) {
	s, store, _ := completionFixture(t)
	backup := s.Backup
	s.Backup = func(context.Context) (string, error) { return "", errors.New("restore unavailable") }
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 1 || store.operations[0].Kind != "access_notification" {
		t.Fatal("backup failure not reported")
	}
	s.Backup = backup
	store.messages = []domain.Message{{ID: 1, TicketID: 1, Text: "before edit", CreatedAt: s.cleanupNow().Add(-time.Minute)}}
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.messages[0].Text = "late edit"
	if err := s.BeforeDelete(context.Background(), nil, store.ticket, -1001, 10); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("late edit accepted by proof: %v", err)
	}
	data, _ := json.Marshal(map[string]string{"key": "completion:1:3:-1001:10"})
	store.events = append(store.events, domain.Event{ID: 5, Kind: "cleanup_deferred", Data: data, CreatedAt: s.cleanupNow()})
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.operations[len(store.operations)-1].Kind != "delete_topic" {
		t.Fatal("new proof retry not enqueued")
	}
}
func TestMissingAttachmentBlocksBackupAndDeletion(t *testing.T) {
	s, store, calls := completionFixture(t)
	store.messages = []domain.Message{{ID: 1, FileID: "telegram-file", CreatedAt: s.cleanupNow()}}
	if err := s.CleanupTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || len(store.operations) != 1 || store.operations[0].Kind != "access_notification" {
		t.Fatal("missing archive permitted deletion")
	}
}

type cleanupFenceProbe struct {
	*fakeStore
	calls int
}

func (p *cleanupFenceProbe) Events(context.Context, int64) ([]domain.Event, error) {
	return nil, errors.New("borrowed a second database connection")
}
func (p *cleanupFenceProbe) Messages(context.Context, int64) ([]domain.Message, error) {
	return nil, errors.New("borrowed a second database connection")
}
func (p *cleanupFenceProbe) CleanupFence(context.Context, domain.Tx, int64) error {
	p.calls++
	return nil
}

type cleanupProofTx struct {
	domain.Tx
	store *fakeStore
}

func (p *cleanupProofTx) Events(context.Context, int64) ([]domain.Event, error) {
	return p.store.events, nil
}
func (p *cleanupProofTx) Messages(context.Context, int64) ([]domain.Message, error) {
	return p.store.messages, nil
}
func TestCleanupHashesProofBeforeGlobalFenceUsingTransactionConnection(t *testing.T) {
	s, store, _ := completionFixture(t)
	ctx := t.Context()
	if err := s.CleanupTick(ctx); err != nil {
		t.Fatal(err)
	}
	probe := &cleanupFenceProbe{fakeStore: store}
	s.Store = probe
	tx := &cleanupProofTx{store: store}
	if err := s.BeforeDelete(ctx, tx, store.ticket, -1001, 10); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 {
		t.Fatal("acceptance fence missing")
	}
	dir := proofDirectory(store.events, "completion:1:3:-1001:10")
	if err := os.WriteFile(filepath.Join(dir, "database.dump"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	probe.calls = 0
	if err := s.BeforeDelete(ctx, tx, store.ticket, -1001, 10); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if probe.calls != 0 {
		t.Fatal("global fence held during proof hashing")
	}
}
