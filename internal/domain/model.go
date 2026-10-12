package domain

import (
	"context"
	"encoding/json"
	"time"
)

type Department string

const (
	IT           Department = "it"
	OneC         Department = "1c"
	Installation Department = "installation"
	Field        Department = "field"
)

func (d Department) Valid() bool { return d == IT || d == OneC || d == Installation || d == Field }

type Status string

const (
	New        Status = "new"
	InProgress Status = "in_progress"
	Waiting    Status = "waiting_client"
	Closed     Status = "closed"
	Cancelled  Status = "cancelled"
)

func (s Status) Active() bool { return s == New || s == InProgress || s == Waiting }

type User struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	CompanyID   int64        `json:"company_id"`
	Role        string       `json:"role"`
	Approved    bool         `json:"approved"`
	Departments []Department `json:"departments"`
}
type Company struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type AccessRequest struct {
	ID        int64
	UserID    int64
	CompanyID int64
	Status    string
	CreatedAt time.Time
}
type Ticket struct {
	ID                int64      `json:"id"`
	ClientID          int64      `json:"client_id"`
	CompanyID         int64      `json:"company_id"`
	Department        Department `json:"department"`
	Description       string     `json:"description"`
	Status            Status     `json:"status"`
	AssigneeID        int64      `json:"assignee_id"`
	Version           int64      `json:"version"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	FirstResponseAt   *time.Time `json:"first_response_at,omitempty"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	ClosedBy          int64      `json:"closed_by"`
	Resolution        string     `json:"resolution"`
	TopicChatID       int64      `json:"topic_chat_id"`
	TopicID           int64      `json:"topic_id"`
	QueueMessageID    int64      `json:"queue_message_id"`
	TopicMessageID    int64      `json:"topic_message_id"`
	RoutingPending    bool       `json:"routing_pending"`
	RoutingGeneration int64      `json:"routing_generation"`
}
type MessageRevision struct {
	Text     string
	EditedAt time.Time
}
type Message struct {
	DeferredDelivery  bool       `json:"deferred_delivery,omitempty"`
	RoutingDepartment Department `json:"routing_department"`
	ID                int64
	TicketID          int64
	AuthorID          int64
	Direction         string
	Text              string
	TelegramChatID    int64
	TelegramMessageID int64
	ReplyToMessageID  int64
	FileID            string
	FileName          string
	FileSize          int64
	MediaType         string
	MediaGroupID      string
	ArchivePath       string
	SHA256            string
	CreatedAt         time.Time
	EditedAt          *time.Time
	Revisions         []MessageRevision
}
type Event struct {
	ID        int64
	TicketID  int64
	ActorID   int64
	Kind      string
	Data      json.RawMessage
	CreatedAt time.Time
}
type Operation struct {
	ID          int64
	Kind        string
	TicketID    int64
	Payload     json.RawMessage
	Attempts    int
	AvailableAt time.Time
	LastError   string
	State       string
}

// Tx methods execute on the same database transaction. GetTicket and GetUser
// lock their rows; implementations serialize user creation and ticket changes.
type Tx interface {
	GetUser(context.Context, int64) (User, error)
	SaveUser(context.Context, User) error
	GetTicket(context.Context, int64) (Ticket, error)
	ActiveTicket(context.Context, int64) (Ticket, error)
	SaveTicket(context.Context, *Ticket) error
	AddMessage(context.Context, *Message) error
	EditMessage(context.Context, *Message) error
	AddEvent(context.Context, Event) error
	Enqueue(context.Context, Operation) error
	RequestAccess(context.Context, int64, int64) error
	ResolveAccess(context.Context, int64, bool) (AccessRequest, error)
}
type Store interface {
	Transact(context.Context, func(Tx) error) error
	User(context.Context, int64) (User, error)
	Companies(context.Context) ([]Company, error)
	Ticket(context.Context, int64) (Ticket, error)
	ActiveTicket(context.Context, int64) (Ticket, error)
	Tickets(context.Context) ([]Ticket, error)
	TopicTicket(context.Context, int64, int64) (Ticket, error)
	Messages(context.Context, int64) ([]Message, error)
	Events(context.Context, int64) ([]Event, error)
	AccessRequests(context.Context) ([]AccessRequest, error)
	SaveUpdate(context.Context, int64, json.RawMessage) (bool, error)
	PendingUpdates(context.Context, int) ([]StoredUpdate, error)
	FinishUpdate(context.Context, int64) error
	LastUpdateID(context.Context) (int64, error)
	NextOperation(context.Context) (Operation, error)
	FinishOperation(context.Context, int64) error
	RetryOperation(context.Context, int64, string, time.Duration, bool) error
	SaveArchive(context.Context, int64, string, string) error
	Ping(context.Context) error
}
type StoredUpdate struct {
	ID      int64
	Payload json.RawMessage
}
