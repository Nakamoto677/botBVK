package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

// An "inline keyboard" with no rows is not a keyboard, and Telegram answers
// `field "inline_keyboard" must be of type Array` for it, failing the whole request.
// Callers legitimately build an empty keyboard to strip buttons off a finished card,
// so the field is dropped at the source instead of relying on every caller to check.
func TestEmptyMarkupSerialisesWithoutTheKeyboardField(t *testing.T) {
	for name, buttons := range map[string][][]Button{
		"nil":        nil,
		"empty":      {},
		"empty row":  {{}},
		"empty rows": {{}, {}},
	} {
		raw, err := json.Marshal(Markup{Buttons: buttons})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "inline_keyboard") {
			t.Fatalf("%s keyboard serialised to %s", name, raw)
		}
		if string(raw) != "{}" {
			t.Fatalf("%s keyboard serialised to %s, want {}", name, raw)
		}
	}
}

// A keyboard that does have buttons must be unaffected.
func TestRealMarkupKeepsItsRows(t *testing.T) {
	raw, err := json.Marshal(Markup{Buttons: [][]Button{{{Text: "Взять", Data: "claim:1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"inline_keyboard":[[`) {
		t.Fatalf("real keyboard lost: %s", raw)
	}
}
