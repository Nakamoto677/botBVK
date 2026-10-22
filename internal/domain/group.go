package domain

// GroupTicketRequest is one explicit request in a company group chat. The
// group is the client identity: ChatID becomes Ticket.ClientID, while Requester
// records the person who actually wrote the trigger message.
type GroupTicketRequest struct {
	ChatID            int64
	ChatTitle         string
	CompanyID         int64
	RequesterID       int64
	RequesterName     string
	RequesterUsername string
	Department        Department
	Urgency           Urgency
	Text              string
	SourceMessageID   int64
	Attachment        *Message
}

// GroupAuthor describes the sender of a message in a company group chat. A
// missing user record is created without private-chat access.
type GroupAuthor struct {
	ID       int64
	Name     string
	Username string
}

// DefaultDepartmentTags returns the shipped aliases for a department. The
// installation department keeps its English alias because clients often copy
// the department name from an integration or a document.
func DefaultDepartmentTags(d Department) []string {
	switch d {
	case OneC:
		return []string{"#1c", "#1с"}
	case IT:
		return []string{"#it", "#ит"}
	case Installation:
		return []string{"#монтаж", "#installation"}
	case Field:
		return []string{"#выезд", "#field"}
	default:
		return nil
	}
}

// UrgencyTags are shared by every department and are stripped from the ticket
// description together with the department tag.
func UrgencyTags() []string { return []string{"#срочно", "#urgent"} }
