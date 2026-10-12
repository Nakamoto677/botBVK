package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"admin_id":1,"staff":[{"id":2,"role":"owner"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid role accepted")
	}
	if err := os.WriteFile(path, []byte(`{"admin_id":1}`), 0600); err != nil {
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
