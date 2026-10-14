package analytics

import (
	"encoding/json"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, tickets.DefaultCalendar().Location)
}
func event(n int64, kind string, when time.Time, actor int64, data string) domain.Event {
	return domain.Event{ID: n, TicketID: 1, Kind: kind, CreatedAt: when, ActorID: actor, Data: json.RawMessage(data)}
}
func options() Options {
	return Options{Calendar: tickets.DefaultCalendar(), FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 2}
}
func value(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("duration got %v want %v", got, want)
	}
}
func requireID(t *testing.T, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("id got %v want %v", got, want)
	}
}

func TestCyclesAssignmentsDepartmentsAndWaiting(t *testing.T) {
	src := Source{At: at(12, 17, 0), Tickets: []domain.Ticket{{ID: 1, ClientID: 100, Department: domain.OneC, Status: domain.Closed, AssigneeID: 40, CreatedAt: at(12, 9, 0), ClosedBy: 999}}, Users: []domain.User{{ID: 100, Name: "Клиент"}}}
	src.Events = []domain.Event{
		event(1, "created", at(12, 9, 0), 100, "null"), event(2, "claimed", at(12, 9, 5), 10, "null"),
		event(3, "status_changed", at(12, 10, 0), 10, `{"status":"waiting_client"}`), event(4, "status_changed", at(12, 11, 0), 10, `{"status":"in_progress"}`),
		event(5, "reassigned", at(12, 11, 15), 10, `{"from":10,"to":20}`), event(6, "closed", at(12, 13, 0), 30, `{"resolution":"Готово","token":"secret"}`),
		event(7, "reopened", at(12, 14, 0), 100, `{"from":"it"}`), event(8, "transferred", at(12, 14, 30), 20, `{"from":"it","to":"1c","reason":"Учёт","text":"private conversation"}`),
		event(9, "claimed", at(12, 14, 40), 40, "null"), event(10, "closed", at(12, 16, 0), 40, `{"resolution":"Исправлено"}`),
	}
	got := Calculate(src, options())
	if len(got.Cycles) != 2 || got.IncompleteCount != 0 {
		t.Fatalf("cycles=%+v tickets=%+v", got.Cycles, got.Tickets)
	}
	a, b := got.Cycles[0], got.Cycles[1]
	requireID(t, a.ClosedBy, 30)
	requireID(t, b.ClosedBy, 40)
	requireID(t, got.Tickets[0].FirstAssigneeID, 10)
	if *a.Department != domain.IT || *b.Department != domain.OneC {
		t.Fatalf("departments %+v %+v", a, b)
	}
	value(t, a.WorkSeconds, 4*3600)
	value(t, a.WaitingWorkSeconds, 3600)
	value(t, a.WorkWithoutWaitingSeconds, 3*3600)
	value(t, b.WorkSeconds, 2*3600)
	if got.Events[5].Detail != "Готово" || got.Events[7].Detail != "Учёт" || got.Tickets[0].ClientName != "Клиент" {
		t.Fatalf("normalization %+v", got)
	}
	if got.Events[0].Status == nil || *got.Events[0].Status != domain.New || got.Events[2].Status == nil || *got.Events[2].Status != domain.Waiting {
		t.Fatalf("event states mutated: %+v", got.Events)
	}
	// Closed time remains an interval for boundary queue reconstruction, but is
	// excluded from the next cycle's resolution duration.
	found := false
	for _, in := range got.Intervals {
		if in.Status == domain.Closed && in.StartedAt.Equal(at(12, 13, 0)) && in.EndedAt != nil && in.EndedAt.Equal(at(12, 14, 0)) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing completed-state interval")
	}
}

func TestFirstResponseRequiresOriginalDeliveredOutboundInCycle(t *testing.T) {
	src := Source{At: at(12, 15, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.InProgress, AssigneeID: 20, CreatedAt: at(12, 9, 0)}}}
	src.Messages = []domain.Message{
		{ID: 1, TicketID: 1, AuthorID: 10, Direction: "outbound", CreatedAt: at(12, 9, 10)},
		{ID: 2, TicketID: 1, AuthorID: 20, Direction: "outbound", CreatedAt: at(12, 12, 10)},
		{ID: 3, TicketID: 1, AuthorID: 20, Direction: "internal", CreatedAt: at(12, 12, 5)},
		{ID: 4, TicketID: 1, AuthorID: 30, Direction: "outbound", CreatedAt: at(12, 12, 15)},
		{ID: 5, TicketID: 1, AuthorID: 20, Direction: "outbound", CreatedAt: at(12, 12, 1)}, // failed delivery
	}
	src.Events = []domain.Event{
		event(1, "created", at(12, 9, 0), 100, "null"), event(2, "claimed", at(12, 9, 1), 10, "null"),
		event(3, "first_response", at(12, 9, 5), 10, "null"), event(4, "message_delivered", at(12, 9, 30), 10, `{"source_message_id":1,"direction":"outbound","edited_at":null}`),
		event(5, "closed", at(12, 10, 0), 10, `{"resolution":"done"}`), event(6, "reopened", at(12, 12, 0), 100, `{"from":"it"}`), event(7, "claimed", at(12, 12, 1), 20, "null"),
		event(8, "message_delivered", at(12, 12, 5), 10, `{"source_message_id":1,"direction":"outbound"}`),
		event(9, "message_delivered", at(12, 12, 6), 20, `{"source_message_id":3,"direction":"internal"}`),
		event(10, "message_delivered", at(12, 12, 12), 20, `{"source_message_id":2,"direction":"outbound","edited_at":"2026-10-12T12:11:00+08:00"}`),
		event(11, "message_delivered", at(12, 12, 20), 30, `{"source_message_id":4,"direction":"outbound"}`),
	}
	got := Calculate(src, options())
	if len(got.Cycles) != 2 {
		t.Fatal(got.Cycles)
	}
	a, b := got.Cycles[0], got.Cycles[1]
	if a.FirstResponseAt == nil || !a.FirstResponseAt.Equal(at(12, 9, 30)) || b.FirstResponseAt == nil || !b.FirstResponseAt.Equal(at(12, 12, 20)) {
		t.Fatalf("response %+v %+v", a, b)
	}
	requireID(t, b.FirstResponseBy, 30)
	value(t, b.FirstResponseWorkSeconds, 20*60)
	requireID(t, got.Tickets[0].FirstResponseBy, 30)
}

func TestCalendarWeekendHolidayAndBoundary(t *testing.T) {
	src := Source{At: at(13, 11, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.Closed, AssigneeID: 10, CreatedAt: at(9, 16, 0)}}}
	src.Events = []domain.Event{event(1, "created", at(9, 16, 0), 100, "null"), event(2, "claimed", at(9, 16, 1), 10, "null"), event(3, "status_changed", at(9, 17, 0), 10, `{"status":"waiting_client"}`), event(4, "status_changed", at(13, 10, 0), 10, `{"status":"in_progress"}`), event(5, "closed", at(13, 11, 0), 10, `{"resolution":"done"}`)}
	opt := options()
	opt.Calendar.Holidays = map[string]bool{"2026-10-12": true}
	got := Calculate(src, opt).Cycles[0]
	value(t, got.CalendarSeconds, 91*3600)
	value(t, got.WorkSeconds, 4*3600)
	value(t, got.WaitingWorkSeconds, 2*3600)
	value(t, got.WorkWithoutWaitingSeconds, 2*3600)
}

func TestMissingHistoryNeverInventsHistoricalOwnerOrDuration(t *testing.T) {
	src := Source{At: at(12, 17, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.Closed, AssigneeID: 99, ClosedBy: 99, CreatedAt: at(12, 9, 0)}}}
	src.Events = []domain.Event{event(1, "closed", at(12, 16, 0), 20, `{"resolution":"done"}`)}
	got := Calculate(src, options())
	if got.IncompleteCount != 1 || !got.Tickets[0].Incomplete || got.Tickets[0].FirstAssigneeID != nil {
		t.Fatalf("missing flags %+v", got)
	}
	requireID(t, got.Cycles[0].ClosedBy, 20)
	if got.Cycles[0].WorkSeconds != nil || got.Cycles[0].Department != nil {
		t.Fatalf("invented facts %+v", got.Cycles[0])
	}
	for _, in := range got.Intervals {
		if in.AssigneeID != nil {
			t.Fatalf("invented owner %+v", in)
		}
	}
	// No events also yields a clearly incomplete unknown interval.
	src.Events = nil
	got = Calculate(src, options())
	if got.IncompleteCount != 1 || got.Intervals[0].Department != nil || got.Intervals[0].Status != "" {
		t.Fatalf("invented history %+v", got)
	}
}

func TestAttentionBaselinesAndReopenedResponse(t *testing.T) {
	old := at(9, 10, 0)
	src := Source{At: at(12, 16, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.InProgress, AssigneeID: 20, CreatedAt: at(9, 9, 0), FirstResponseAt: &old}}}
	src.Events = []domain.Event{event(1, "created", at(9, 9, 0), 100, "null"), event(2, "claimed", at(9, 9, 5), 10, "null"), event(3, "closed", at(9, 11, 0), 10, `{"resolution":"done"}`), event(4, "reopened", at(12, 9, 0), 100, `{"from":"it"}`), event(5, "claimed", at(12, 9, 5), 20, "null")}
	src.Messages = []domain.Message{{ID: 1, TicketID: 1, Direction: "inbound", CreatedAt: at(12, 15, 0)}, {ID: 2, TicketID: 1, Direction: "outbound", AuthorID: 20, CreatedAt: at(12, 11, 0)}}
	got := Calculate(src, options()).Tickets[0]
	if !got.ResponseOverdue || !got.IdleOverdue || got.FirstResponseAt != nil {
		t.Fatalf("attention %+v", got)
	}
	if got.LastActivityAt == nil || !got.LastActivityAt.Equal(at(12, 11, 0)) {
		t.Fatalf("baseline %+v", got)
	}
	value(t, got.IdleWorkSeconds, 5*3600)
	src.Tickets[0].Status = domain.Waiting
	src.Events = append(src.Events, event(6, "status_changed", at(12, 12, 0), 20, `{"status":"waiting_client"}`))
	got = Calculate(src, options()).Tickets[0]
	if got.IdleOverdue || got.WaitingOverdue || got.WaitingSince == nil || !got.WaitingSince.Equal(at(12, 12, 0)) {
		t.Fatalf("waiting %+v", got)
	}
	value(t, got.WaitingWorkSeconds, 4*3600)
}

func TestCancellationStableOrderingFutureFactsAndEmptySource(t *testing.T) {
	src := Source{At: at(12, 12, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.Cancelled, CreatedAt: at(12, 9, 0)}}}
	src.Events = []domain.Event{event(3, "closed", at(12, 15, 0), 99, `{"resolution":"future"}`), event(2, "cancelled", at(12, 10, 0), 100, "null"), event(1, "created", at(12, 9, 0), 100, "null")}
	got := Calculate(src, options())
	if len(got.Events) != 2 || got.Events[0].Kind != "created" || got.Cycles[0].Outcome != "cancelled" || got.IncompleteCount != 0 {
		t.Fatalf("cancellation %+v", got)
	}
	requireID(t, got.Cycles[0].ClosedBy, 100)
	value(t, got.Cycles[0].WorkSeconds, 3600)
	empty := Calculate(Source{At: at(12, 12, 0)}, options())
	if len(empty.Tickets) != 0 || len(empty.Cycles) != 0 || empty.IncompleteCount != 0 {
		t.Fatal(empty)
	}
}

func TestBrokenTransferAndExplicitReopenAnchor(t *testing.T) {
	src := Source{At: at(12, 16, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.OneC, Status: domain.InProgress, AssigneeID: 20, CreatedAt: at(12, 9, 0)}}}
	src.Events = []domain.Event{event(1, "created", at(12, 9, 0), 100, "null"), event(2, "claimed", at(12, 9, 10), 10, "null"), event(3, "transferred", at(12, 10, 0), 10, `{"to":"1c"}`), event(4, "claimed", at(12, 10, 10), 20, "null")}
	got := Calculate(src, options())
	if !got.Tickets[0].Incomplete || got.Events[0].Department != nil || got.Cycles[0].WorkSeconds != nil {
		t.Fatalf("broken transfer invented history: %+v", got)
	}
	// A known reopen department anchors the preceding closure even if the
	// original creation record is absent. It also establishes a valid new cycle.
	src.Events = []domain.Event{event(1, "closed", at(12, 10, 0), 10, `{"resolution":"done"}`), event(2, "reopened", at(12, 11, 0), 100, `{"from":"1c"}`), event(3, "claimed", at(12, 11, 10), 20, "null"), event(4, "message_delivered", at(12, 11, 30), 30, `{"source_message_id":1}`)}
	src.Messages = []domain.Message{{ID: 1, TicketID: 1, AuthorID: 30, Direction: "outbound", CreatedAt: at(12, 11, 20)}}
	got = Calculate(src, options())
	if got.Events[0].Department == nil || *got.Events[0].Department != domain.OneC || len(got.Cycles) != 2 {
		t.Fatalf("lost explicit anchor %+v", got)
	}
	if got.Cycles[0].WorkSeconds != nil || got.Cycles[1].FirstResponseAt == nil {
		t.Fatalf("partial reconstruction %+v", got.Cycles)
	}
	value(t, got.Cycles[1].WorkSeconds, 5*3600)
}

func TestResponseDepartmentBeforeLaterTransfer(t *testing.T) {
	src := Source{At: at(12, 16, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.OneC, Status: domain.Closed, AssigneeID: 20, CreatedAt: at(12, 9, 0)}}}
	src.Events = []domain.Event{event(1, "created", at(12, 9, 0), 100, "null"), event(2, "claimed", at(12, 9, 5), 10, "null"), event(3, "message_delivered", at(12, 9, 30), 10, `{"source_message_id":1,"direction":"outbound"}`), event(4, "transferred", at(12, 10, 0), 10, `{"from":"it","to":"1c"}`), event(5, "claimed", at(12, 10, 5), 20, "null"), event(6, "closed", at(12, 12, 0), 20, `{"resolution":"done"}`)}
	src.Messages = []domain.Message{{ID: 1, TicketID: 1, AuthorID: 10, Direction: "outbound", CreatedAt: at(12, 9, 20)}}
	c := Calculate(src, options()).Cycles[0]
	if c.FirstResponseDepartment == nil || *c.FirstResponseDepartment != domain.IT || c.Department == nil || *c.Department != domain.OneC {
		t.Fatalf("historical reply department %+v", c)
	}
}

func TestInitialDescriptionDeliveryIsKnownServiceMetadata(t *testing.T) {
	src := Source{At: at(12, 12, 0), Tickets: []domain.Ticket{{ID: 1, Department: domain.IT, Status: domain.Closed, AssigneeID: 10, CreatedAt: at(12, 9, 0)}}}
	src.Events = []domain.Event{event(1, "created", at(12, 9, 0), 100, "null"), event(2, "claimed", at(12, 9, 5), 10, "null"), event(3, "message_delivered", at(12, 9, 6), 100, `{"source_message_id":0,"direction":"inbound","initial_description":true}`), event(4, "closed", at(12, 10, 0), 10, `{"resolution":"Готово"}`)}
	result := Calculate(src, options())
	if result.IncompleteCount != 0 || result.Events[2].Incomplete || result.Cycles[0].FirstResponseAt != nil {
		t.Fatalf("initial description misclassified: %+v", result)
	}
	value(t, result.Cycles[0].WorkSeconds, 3600)
	// A missing original public reply remains an actual history gap.
	src.Events[2] = event(3, "message_delivered", at(12, 9, 6), 10, `{"source_message_id":999,"direction":"outbound"}`)
	if Calculate(src, options()).IncompleteCount != 1 {
		t.Fatal("missing public reply was ignored")
	}
}

func BenchmarkCalculateFiveYears(b *testing.B) {
	now := at(13, 18, 0)
	src := Source{At: now, Users: []domain.User{{ID: 100, Name: "Клиент"}, {ID: 10, Name: "Инженер"}}}
	// Upper expected traffic: 40/day * 365 * 5, with six facts and one message.
	for n := int64(1); n <= 73000; n++ {
		start := now.AddDate(0, 0, -int((n-1)/40)).Add(-8 * time.Hour)
		src.Tickets = append(src.Tickets, domain.Ticket{ID: n, ClientID: 100, Department: domain.IT, Status: domain.Closed, AssigneeID: 10, CreatedAt: start})
		kinds := []string{"created", "claimed", "message_delivered", "status_changed", "status_changed", "closed"}
		data := []string{"null", "null", `{"source_message_id":1,"direction":"outbound"}`, `{"status":"waiting_client"}`, `{"status":"in_progress"}`, `{"resolution":"Готово"}`}
		for j, k := range kinds {
			src.Events = append(src.Events, domain.Event{ID: n*10 + int64(j), TicketID: n, ActorID: 10, Kind: k, CreatedAt: start.Add(time.Duration(j) * 10 * time.Minute), Data: json.RawMessage(data[j])})
		}
		src.Messages = append(src.Messages, domain.Message{ID: 1, TicketID: n, AuthorID: 10, Direction: "outbound", CreatedAt: start.Add(15 * time.Minute)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result := Calculate(src, options())
		if len(result.Tickets) != 73000 || result.IncompleteCount != 0 {
			b.Fatal(len(result.Tickets), result.IncompleteCount)
		}
	}
}
