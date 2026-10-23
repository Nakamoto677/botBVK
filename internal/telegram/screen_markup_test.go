package telegram

import (
	"context"
	"encoding/json"
	"testing"

	"bot-bvk/internal/domain"
)

// A queued screen carries its keyboard as raw JSON in the operation payload, already
// encoded by the caller. Storing that raw value straight into the API payload made
// encoding/json treat it as []byte and emit a base64 string, so Telegram answered
// `field "inline_keyboard" must be of type Array` and the screen was lost for good.
func TestQueuedScreenSendsItsKeyboardAsAnObject(t *testing.T) {
	for name, markup := range map[string]string{
		"inline keyboard":  `{"inline_keyboard":[[{"text":"Открыть общую","url":"https://t.me/c/1/2"}]]}`,
		"reply keyboard":   `{"keyboard":[[{"text":"Создать заявку"},{"text":"Помощь"}]],"resize_keyboard":true}`,
		"keyboard removal": `{"remove_keyboard":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, capture := capturedClient()
			store := &workerStore{}
			worker := NewWorker(client, store, Config{})
			worker.interval = 0

			payload, err := json.Marshal(map[string]any{
				"user_id":  int64(1),
				"chat_id":  int64(1),
				"text":     "Экран",
				"markup":   json.RawMessage(markup),
				"scenario": "client_home",
			})
			if err != nil {
				t.Fatal(err)
			}
			op := domain.Operation{ID: 1, Kind: "ui_screen", Payload: payload}
			if err := worker.Handle(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if len(capture.bodies) != 1 {
				t.Fatalf("expected one API call, got %d", len(capture.bodies))
			}
			sent, ok := capture.bodies[0]["reply_markup"]
			if !ok {
				t.Fatal("screen sent without a keyboard")
			}
			// The decisive check: after JSON decoding the field must be an object,
			// not the base64 string that []byte produces.
			object, ok := sent.(map[string]any)
			if !ok {
				t.Fatalf("reply_markup was sent as %T, so Telegram sees a string: %v", sent, sent)
			}
			if parsed, ok := object["inline_keyboard"]; ok {
				if _, isArray := parsed.([]any); !isArray {
					t.Fatalf("inline_keyboard is %T, must be an array: %v", parsed, parsed)
				}
			}
			if parsed, ok := object["keyboard"]; ok {
				if _, isArray := parsed.([]any); !isArray {
					t.Fatalf("keyboard is %T, must be an array: %v", parsed, parsed)
				}
			}
		})
	}
}

// A screen with no keyboard must omit reply_markup entirely rather than send an
// empty or null value. This is the shape that actually reached production:
//
//	markup={"inline_keyboard": null}
//
// Telegram answers `field "inline_keyboard" must be of type Array` for it, and the
// check for the literal "null" does not catch it because the payload is an object.
func TestQueuedScreenWithoutKeyboardOmitsTheField(t *testing.T) {
	for _, markup := range []string{"null", `{"inline_keyboard":null}`, `{"inline_keyboard":[]}`, `{}`} {
		t.Run(markup, func(t *testing.T) {
			client, capture := capturedClient()
			worker := NewWorker(client, &workerStore{}, Config{})
			worker.interval = 0
			payload, err := json.Marshal(map[string]any{"user_id": int64(1), "chat_id": int64(1), "text": "Без кнопок", "markup": json.RawMessage(markup)})
			if err != nil {
				t.Fatal(err)
			}
			op := domain.Operation{ID: 2, Kind: "ui_screen", Payload: payload}
			if err := worker.Handle(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if len(capture.bodies) != 1 {
				t.Fatalf("expected one API call, got %d", len(capture.bodies))
			}
			if got, ok := capture.bodies[0]["reply_markup"]; ok {
				t.Fatalf("empty keyboard %q was sent as %v", markup, got)
			}
		})
	}
}

// A keyboard that does carry buttons must still be delivered, so the empty-keyboard
// rule cannot swallow real ones.
func TestQueuedScreenKeepsARealKeyboard(t *testing.T) {
	client, capture := capturedClient()
	worker := NewWorker(client, &workerStore{}, Config{})
	worker.interval = 0
	markup := `{"inline_keyboard":[[{"text":"Взять в работу","callback_data":"claim:7"}]]}`
	payload, err := json.Marshal(map[string]any{"user_id": int64(1), "chat_id": int64(-100), "text": "Карточка", "markup": json.RawMessage(markup)})
	if err != nil {
		t.Fatal(err)
	}
	op := domain.Operation{ID: 3, Kind: "ui_screen", Payload: payload}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	sent, ok := capture.bodies[0]["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("keyboard not delivered: %v", capture.bodies[0]["reply_markup"])
	}
	rows, ok := sent["inline_keyboard"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("keyboard rows wrong: %v", sent["inline_keyboard"])
	}
}
