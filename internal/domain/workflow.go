package domain

import "context"

// Auxiliary messages are only bot-owned temporary UI messages. Ticket cards,
// user messages and transcript attachments must never be registered here.
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
	Auxiliary(context.Context, int64, int64, int64, string) ([]AuxiliaryMessage, error)
	QueueAuxiliaryCleanup(context.Context, int64, int64, int64, string) error
	SaveWorkflow(context.Context, int64, int64, int64, string, []byte) error
	Workflow(context.Context, int64, int64, int64, string) ([]byte, error)
	DeleteWorkflow(context.Context, int64, int64, int64, string) error
}
