package monitor

import (
	"context"
	"testing"
	"time"

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
}

func (f *fakeStore) Tickets(context.Context) ([]domain.Ticket, error) {
	return []domain.Ticket{f.ticket}, nil
}
func (f *fakeStore) Messages(context.Context, int64) ([]domain.Message, error) {
	return f.messages, nil
}
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
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: t.TempDir(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 1, StartHour: 9, EndHour: 18}, Calendar: calendar, Now: func() time.Time { return now }}
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

func TestUrgentAssignmentRemindersResetAfterTransfer(t *testing.T) {
	calendar := tickets.DefaultCalendar()
	loc := calendar.Location
	created := time.Date(2026, 10, 9, 17, 55, 0, 0, loc) // Friday
	now := time.Date(2026, 10, 12, 9, 5, 0, 0, loc)      // Monday
	store := &fakeStore{ticket: domain.Ticket{ID: 12, Version: 1, Status: domain.New, Urgency: domain.Urgent, CreatedAt: created}}
	s := &Scheduler{Store: store, Config: config.Config{FirstResponseMinutes: 60, StartHour: 9, EndHour: 18}, Calendar: calendar, Now: func() time.Time { return now }}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.operations) != 1 || store.operations[0].Kind != "urgent_assignment_reminder" {
		t.Fatalf("10-minute reminder: %+v", store.operations)
	}
	now = time.Date(2026, 10, 12, 9, 15, 0, 0, loc)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 2 {
		t.Fatalf("20-minute reminder: %+v", store.operations)
	}
	store.events = append(store.events, domain.Event{ID: 10, Kind: "transferred", CreatedAt: now})
	now = now.Add(10 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 3 || store.operations[2].Kind != "urgent_assignment_reminder" {
		t.Fatalf("new department reminder: %+v", store.operations)
	}
	store.ticket.AssigneeID = 4
	store.ticket.Status = domain.InProgress
	now = now.Add(35 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 4 || store.operations[3].Kind != "first_response_reminder" {
		t.Fatalf("first response must still be tracked: %+v", store.operations)
	}
}
func TestNoIdleReminderForInactiveEngineer(t *testing.T) {
	calendar := tickets.DefaultCalendar()
	loc := calendar.Location
	created := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	first := created.Add(10 * time.Minute)
	store := &fakeStore{ticket: domain.Ticket{ID: 1, Version: 1, Status: domain.InProgress, CreatedAt: created, FirstResponseAt: &first}}
	// Almost a whole working week without engineer activity: the idle reminder
	// was removed and must not enqueue anything.
	now := time.Date(2026, 10, 16, 17, 0, 0, 0, loc)
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: t.TempDir(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 1, StartHour: 9, EndHour: 18}, Calendar: calendar, Now: func() time.Time { return now }}
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.operations) != 0 {
		t.Fatalf("idle engineer reminder must stay removed, got %+v", store.operations)
	}
}
func TestWaitingClientReminderAfterOneWorkingDay(t *testing.T) {
	calendar := tickets.DefaultCalendar()
	loc := calendar.Location
	created := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	store := &fakeStore{ticket: domain.Ticket{ID: 7, Version: 1, Status: domain.Waiting, CreatedAt: created, FirstResponseAt: &created}}
	now := time.Date(2026, 10, 12, 17, 0, 0, 0, loc)
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: t.TempDir(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 1, StartHour: 9, EndHour: 18}, Calendar: calendar, Now: func() time.Time { return now }}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 0 {
		t.Fatal("client reminder fired before one working day")
	}
	// Tuesday 09:00 is exactly one nine-hour working day after Monday 09:00.
	now = time.Date(2026, 10, 13, 9, 0, 0, 0, loc)
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.operations) != 1 || store.operations[0].Kind != "notify_client" {
		t.Fatalf("expected one client reminder after one working day, got %+v", store.operations)
	}
}

func TestWaitingReminderSkippedForGroupTicket(t *testing.T) {
	calendar := tickets.DefaultCalendar()
	loc := calendar.Location
	created := time.Date(2026, 10, 12, 10, 0, 0, 0, loc)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, loc)
	store := &fakeStore{ticket: domain.Ticket{ID: 7, Version: 1, Channel: domain.ChannelGroup, ClientID: -100500, Status: domain.Waiting, CreatedAt: created, FirstResponseAt: &created}}
	s := &Scheduler{Store: store, Config: config.Config{BackupDir: t.TempDir(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 1, StartHour: 9, EndHour: 18}, Calendar: calendar, Now: func() time.Time { return now }}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.operations) != 0 {
		t.Fatalf("group ticket received a private reminder: %+v", store.operations)
	}
}
