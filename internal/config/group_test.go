package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"bot-bvk/internal/domain"
)

func loadConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestDepartmentTagsDefaultAndNormalize(t *testing.T) {
	c, err := loadConfig(t, `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2},"1c":{"chat_id":-1002,"queue_topic_id":2,"tags":["#1C"," #1С "]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Departments[domain.IT].Tags; len(got) != 2 || got[0] != "#it" || got[1] != "#ит" {
		t.Fatalf("default IT tags: %v", got)
	}
	if got := c.Departments[domain.OneC].Tags; len(got) != 2 || got[0] != "#1c" || got[1] != "#1с" {
		t.Fatalf("normalized 1C tags: %v", got)
	}
}

func TestDepartmentTagsRejectDuplicatesAndUrgency(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate across departments": `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2,"tags":["#same"]},"1c":{"chat_id":-1002,"queue_topic_id":2,"tags":["#same"]}}}`,
		"duplicate inside department":  `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2,"tags":["#it","#it"]}}}`,
		"urgency reserved":             `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2,"tags":["#срочно"]}}}`,
		"invalid tag":                  `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2,"tags":["#it-two"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(t, body); err == nil {
				t.Fatal("invalid tags accepted")
			}
		})
	}
}

func TestClientGroupsValidation(t *testing.T) {
	base := `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2}},"client_groups":`
	for name, body := range map[string]string{
		"positive chat":     base + `[{"chat_id":10,"company":"x"}]}`,
		"duplicate chat":    base + `[{"chat_id":-2000,"company":"x"},{"chat_id":-2000,"company":"y"}]}`,
		"department chat":   base + `[{"chat_id":-1001,"company":"x"}]}`,
		"empty company":     base + `[{"chat_id":-2000,"company":"  "}]}`,
		"invalid link":      base + `[{"chat_id":-2000,"company":"x","link":"http://example.org"}]}`,
		"missing link okay": base + `[{"chat_id":-2000,"company":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(t, body)
			wantError := name != "missing link okay"
			if (err != nil) != wantError {
				t.Fatalf("err=%v, wantError=%t", err, wantError)
			}
		})
	}
}

func TestClientGroupsDecode(t *testing.T) {
	c, err := loadConfig(t, `{"admin_id":1,"companies":[],"departments":{"it":{"chat_id":-1001,"queue_topic_id":2}},"client_groups":[{"chat_id":-2000,"company":"example","link":"https://t.me/+invite"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c.ClientGroups)
	if string(raw) != `[{"chat_id":-2000,"company":"example","link":"https://t.me/+invite"}]` {
		t.Fatalf("decoded: %s", raw)
	}
}
