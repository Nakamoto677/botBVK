package domain

import "context"

// Auxiliary messages identify disposable UI, retained client milestones, and
// validated engineer control messages. Ticket cards, free text and attachments
// must never be registered here.
type AuxiliaryMessage struct {
	UserID    int64  `json:"user_id"`
	ChatID    int64  `json:"chat_id"`
	TopicID   int64  `json:"topic_id"`
	Scenario  string `json:"scenario"`
	MessageID int64  `json:"message_id"`
}

// WorkflowStore optionally persists UI state without expanding the core Store
// contract used by ticket services and lightweight test doubles.
type WorkflowStore interface {
	SaveAuxiliary(context.Context, AuxiliaryMessage) error
	QueueAuxiliaryCleanup(context.Context, int64, int64, int64, string) error
	SaveWorkflow(context.Context, int64, int64, int64, string, []byte) error
	Workflow(context.Context, int64, int64, int64, string) ([]byte, error)
	DeleteWorkflow(context.Context, int64, int64, int64, string) error
}

// Confirmed sends survive retries; an unknown Telegram network outcome may
// still duplicate a part and is deliberately not described as exactly-once.
type SendProgressStore interface {
	SendPart(context.Context, string, int) (int64, error)
	SaveSendPart(context.Context, string, int, int64) error
}
