package config

import (
	"os"
	"path/filepath"
	"testing"

	"bot-bvk/internal/domain"
)

func TestValidateTelegramWithThreeDepartments(t *testing.T) {
	c := Config{Token: "test", Departments: map[domain.Department]Team{
		domain.IT:    {ChatID: -1001, QueueTopicID: 2},
		domain.OneC:  {ChatID: -1002, QueueTopicID: 3},
		domain.Field: {ChatID: -1003, QueueTopicID: 4},
	}}
	if err := c.ValidateTelegram(); err != nil {
		t.Fatalf("three configured departments rejected: %v", err)
	}
	c.Departments[domain.Field] = Team{ChatID: 1003, QueueTopicID: 4}
	if err := c.ValidateTelegram(); err == nil {
		t.Fatal("positive department chat ID accepted")
	}
}

func TestDatabaseURLFileOverridesStaleEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/old")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "database.url"), []byte("postgres://localhost/pilot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"admin_id":1,"companies":[],"database_url_file":"database.url"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://localhost/pilot" {
		t.Fatal("stale environment won over database_url_file")
	}
}

func TestLoadValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin_id":1,"companies":[],"staff":[{"id":2,"role":"owner"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid role accepted")
	}
	if err := os.WriteFile(path, []byte(`{"admin_id":1,"companies":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.StartHour != 9 || c.EndHour != 18 {
		t.Fatal("wrong defaults")
	}
	if c.ValidateTelegram() == nil {
		t.Fatal("missing token accepted")
	}
}

func TestRemovedRoutingSettingsDoNotBreakExistingConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"admin_id":1,"companies":[],"dispatcher_chat_id":-1009,"departments":{"it":{"chat_id":-1001,"queue_topic_id":2,"duty_user_id":12}}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatalf("legacy settings prevented upgrade: %v", err)
	}
	if config.Departments["it"].ChatID != -1001 {
		t.Fatal("upgrade changed department routing")
	}
	if err := os.WriteFile(path, []byte(`{"admin_id":1,"companies":[],"unrecognized_setting":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("unrecognized setting silently accepted")
	}
}
