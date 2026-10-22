package telegram

import (
	"errors"
	"testing"
)

// A card whose message or whole topic has already been deleted is a normal outcome of
// the cleanup that removes finished topics, not a failure. Treating these as permanent
// errors made the worker mark refreshes dead and the failures piled up until readiness
// reported not-ready. Both descriptions below appear in the production outbox.
func TestEditsOnDeletedCardsCountAsDone(t *testing.T) {
	for _, description := range []string{
		"Bad Request: message to edit not found",
		"Bad Request: message thread not found",
		"Bad Request: TOPIC_ID_INVALID",
		"Bad Request: message is not modified",
	} {
		err := messageEditError(&APIError{Code: 400, Description: description})
		if err != nil {
			t.Fatalf("%q was treated as a failure: %v", description, err)
		}
	}
}

// Everything else must keep failing loudly. A blocked chat or a missing permission is a
// real problem the operator has to see.
func TestRealEditFailuresStillFail(t *testing.T) {
	for _, api := range []*APIError{
		{Code: 403, Description: "Forbidden: bot was blocked by the user"},
		{Code: 400, Description: "Bad Request: chat not found"},
		{Code: 429, Description: "Too Many Requests", RetryAfter: 5},
		{Code: 500, Description: "Internal Server Error"},
	} {
		if err := messageEditError(api); err == nil {
			t.Fatalf("%d %q was swallowed by the edit classifier", api.Code, api.Description)
		}
	}
	// A non-API error must pass straight through.
	plain := errors.New("connection reset")
	if err := messageEditError(plain); !errors.Is(err, plain) {
		t.Fatal("a transport error was swallowed")
	}
}
