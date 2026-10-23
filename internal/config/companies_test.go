package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompanyFormatsAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	for _, tt := range []struct {
		name, value string
		valid       bool
	}{
		{"legacy", `["Alpha","Beta"]`, true},
		{"stable", `[{"id":"alpha","name":"Alpha","legacy_name":"Old Alpha"}]`, true},
		{"mixed", `["Alpha",{"id":"beta","name":"Beta"}]`, true},
		{"empty", `[]`, true},
		{"null", `null`, false},
		{"missing id", `[{"name":"Alpha"}]`, false},
		{"blank", `["  "]`, false},
		{"duplicate name", `["Alpha","Alpha"]`, false},
		{"duplicate id", `[{"id":"a","name":"Alpha"},{"id":"a","name":"Beta"}]`, false},
		{"duplicate alias", `[{"id":"a","name":"Alpha","legacy_name":"Old"},{"id":"b","name":"Beta","legacy_name":"Old"}]`, false},
		{"unknown", `[{"id":"a","name":"Alpha","typo":1}]`, false},
		{"reserved", `[{"id":"legacy:bad","name":"Alpha"}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"admin_id":1,"companies":`+tt.value+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
			if tt.valid && len(c.Companies) > 0 && c.Companies[0].Key == "" {
				t.Fatal("missing normalized identity")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"admin_id":1}`), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("missing catalog accepted")
	}
}

func TestCompanyAccessCodesAreRequiredAndUniqueForBot(t *testing.T) {
	companies := Companies{{Key: "a", Name: "A", AccessCode: "00000-00000-00000-00000-000000"}, {Key: "b", Name: "B", AccessCode: "11111-11111-11111-11111-111111"}}
	codes, err := companies.AccessCodes()
	if err != nil || len(codes) != 2 {
		t.Fatalf("valid codes rejected: %v", err)
	}
	companies[1].AccessCode = companies[0].AccessCode
	if _, err := companies.AccessCodes(); err == nil {
		t.Fatal("duplicate code accepted")
	}
	companies[1].AccessCode = ""
	if _, err := companies.AccessCodes(); err == nil {
		t.Fatal("missing code accepted")
	}
}
