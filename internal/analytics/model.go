package analytics

import (
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
	"time"
)

const Version = "1"

// Source is a consistent, read-only snapshot. Messages never leave the calculator.
type Source struct {
	At        time.Time
	Tickets   []domain.Ticket
	Events    []domain.Event
	Messages  []domain.Message
	Users     []domain.User
	Companies []domain.Company
}

type Ticket struct {
	ID, ClientID, CompanyID                                                                   int64
	Description                                                                               string
	ClientName                                                                                string
	Status                                                                                    domain.Status
	Department                                                                                domain.Department
	AssigneeID, FirstAssigneeID                                                               *int64
	CreatedAt, UpdatedAt                                                                      time.Time
	FirstResponseAt                                                                           *time.Time
	FirstResponseBy                                                                           *int64
	CycleStartedAt                                                                            *time.Time
	LastActivityAt, WaitingSince                                                              *time.Time
	FirstResponseWorkSeconds, ResponseWaitingWorkSeconds, IdleWorkSeconds, WaitingWorkSeconds *float64
	NoAssignee, ResponseOverdue, IdleOverdue, WaitingOverdue                                  bool
	Incomplete                                                                                bool
	Quality                                                                                   string
}

type Cycle struct {
	TicketID                                                                                                                                    int64
	Number                                                                                                                                      int
	StartedAt                                                                                                                                   time.Time
	EndedAt                                                                                                                                     *time.Time
	Outcome                                                                                                                                     string
	Department                                                                                                                                  *domain.Department
	ClosedBy                                                                                                                                    *int64
	Resolution                                                                                                                                  string
	FirstResponseAt                                                                                                                             *time.Time
	FirstResponseBy                                                                                                                             *int64
	FirstResponseDepartment                                                                                                                     *domain.Department
	CalendarSeconds, WorkSeconds, WaitingSeconds, WaitingWorkSeconds, WorkWithoutWaitingSeconds, FirstResponseSeconds, FirstResponseWorkSeconds *float64
	Incomplete                                                                                                                                  bool
}

type Event struct {
	ID, TicketID                             int64
	Kind                                     string
	At                                       time.Time
	ActorID                                  *int64
	Department, FromDepartment, ToDepartment *domain.Department
	AssigneeID                               *int64
	Status                                   *domain.Status
	CycleNumber                              int
	Detail                                   string
	Incomplete                               bool
}

type Interval struct {
	TicketID    int64
	StartedAt   time.Time
	EndedAt     *time.Time
	Status      domain.Status
	Department  *domain.Department
	AssigneeID  *int64
	CycleNumber int
	Incomplete  bool
}

type Snapshot struct {
	At              time.Time
	Tickets         []Ticket
	Cycles          []Cycle
	Events          []Event
	Intervals       []Interval
	Users           []domain.User
	Companies       []domain.Company
	IncompleteCount int
}

type Options struct {
	Calendar                                     tickets.Calendar
	FirstResponseMinutes, IdleHours, WaitingDays int
}

func OptionsFromConfig(c config.Config) Options {
	cal := tickets.DefaultCalendar()
	cal.StartHour, cal.EndHour = c.StartHour, c.EndHour
	cal.Holidays = map[string]bool{}
	for _, day := range c.Holidays {
		cal.Holidays[day] = true
	}
	return Options{cal, c.FirstResponseMinutes, c.IdleHours, c.WaitingDays}
}
