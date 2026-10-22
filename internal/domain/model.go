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

// Departments lists every department in client-facing display order. Button rows
// and validation both derive from this slice so the order cannot drift between
// the form, the transfer prompt and the registration catalog.
func Departments() []Department { return []Department{OneC, IT, Installation, Field} }

func (d Department) Name() string {
	switch d {
	case OneC:
		return "1С"
	case IT:
		return "IT"
	case Installation:
		return "Монтаж"
	case Field:
		return "Выезд"
	default:
		return "Поддержка"
	}
}

// Channel identifies where a ticket was opened. An empty value is the legacy
// and current private-chat flow; ChannelGroup is the company group chat.
type Channel string

const ChannelGroup Channel = "group"

type Status string

type Urgency string

const (
	Urgent  Urgency = "urgent"
	Routine Urgency = "routine"
)

func (u Urgency) Valid() bool { return u == Urgent || u == Routine }

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
	Username    string       `json:"username,omitempty"`
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
	ID      int64   `json:"id"`
	Urgency Urgency `json:"urgency,omitempty"`
	// ClientID is the Telegram user for a private ticket and the negative
	// Telegram chat ID of the company group for a group ticket. The existing
	// bvk_active_client index therefore keeps one active group ticket per chat.
	ClientID          int64      `json:"client_id"`
	Channel           Channel    `json:"channel,omitempty"`
	RequesterID       int64      `json:"requester_id,omitempty"`
	RequesterName     string     `json:"requester_name,omitempty"`
	RequesterUsername string     `json:"requester_username,omitempty"`
	SourceMessageID   int64      `json:"source_message_id,omitempty"`
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
	QueueMediaType    string     `json:"queue_media_type,omitempty"`
	TopicMessageID    int64      `json:"topic_message_id"`
	RoutingPending    bool       `json:"routing_pending"`
	RoutingGeneration int64      `json:"routing_generation"`
}

// IsGroup reports whether the ticket is a conversation in a company group chat
// rather than a private chat with a client.
func (t Ticket) IsGroup() bool { return t.Channel == ChannelGroup }

// ClientTopic is the durable Telegram conversation for a client in one department.
// Ticket IDs and lifecycle remain independent of this binding.
type ClientTopic struct {
	ClientID   int64
	Department Department
	ChatID     int64
	TopicID    int64
}

type ClientTopicStore interface {
	ClientNumber(context.Context, int64) (int64, error)
	ClientTopic(context.Context, int64, Department) (ClientTopic, error)
	PreviousTopicTicket(context.Context, int64, Department, int64) (Ticket, error)
	ActiveClientTopicTicket(context.Context, int64, Department, int64) (Ticket, error)
}

type ClientTopicTx interface {
	BindClientTopic(context.Context, ClientTopic) error
	ReplaceClientTopic(context.Context, ClientTopic, int64) error
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
	TelegramTopicID   int64
	TelegramMessageID int64
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
	// Companies returns only the currently selectable registration catalog.
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
	ID       int64
	Payload  json.RawMessage
	Attempts int
}

// QuarantinedUpdate is accepted work that exhausted its attempts and can never
// complete on its own. It stays unfinished until an operator releases it and the
// update is processed, and it must never order anyone else: nothing selects it
// again and only an explicit release clears the flag.
type QuarantinedUpdate struct {
	ID        int64
	ActorID   int64
	ChatID    int64
	Attempts  int
	LastError string
}

// QuarantineStore inspects and releases quarantined updates. It is separate from
// Store because the capability is optional for lightweight test doubles.
type QuarantineStore interface {
	QuarantinedUpdates(context.Context) ([]QuarantinedUpdate, error)
	QuarantineCount(context.Context) (int, error)
	ReleaseQuarantine(context.Context, int64) error
}

// Draft generations and steps bind retained form buttons to a particular screen.
type Draft struct {
	Generation  string
	Step        string
	Department  Department
	Urgency     Urgency
	Text        string
	Attachments []Message
	// Source IDs make appending descriptions idempotent across inbox retries.
	MessageIDs []int64
	TicketID   int64
}
type BoundDraftStore interface {
	LoadDraft(context.Context, int64) (Draft, error)
	PutDraft(context.Context, int64, Draft) error
}
type DraftTx interface {
	LoadDraft(context.Context, int64) (Draft, error)
	CompleteDraft(context.Context, int64, string, int64) error
}
type RetryInbox interface {
	RetryUpdate(context.Context, int64, string, time.Duration, bool) error
}
