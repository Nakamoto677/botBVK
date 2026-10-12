package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

type Team struct {
	ChatID       int64 `json:"chat_id"`
	QueueTopicID int64 `json:"queue_topic_id"`
}
type Staff struct {
	ID          int64               `json:"id"`
	Name        string              `json:"name"`
	Role        string              `json:"role"`
	Departments []domain.Department `json:"departments"`
}
type Config struct {
	DatabaseURL          string                     `json:"-"`
	Token                string                     `json:"-"`
	AdminID              int64                      `json:"admin_id"`
	Companies            []string                   `json:"companies"`
	Staff                []Staff                    `json:"staff"`
	Departments          map[domain.Department]Team `json:"departments"`
	ArchiveDir           string                     `json:"archive_dir"`
	BackupDir            string                     `json:"backup_dir"`
	HealthAddress        string                     `json:"health_address"`
	Holidays             []string                   `json:"holidays"`
	StartHour            int                        `json:"start_hour"`
	EndHour              int                        `json:"end_hour"`
	FirstResponseMinutes int                        `json:"first_response_minutes"`
	IdleHours            int                        `json:"idle_hours"`
	WaitingDays          int                        `json:"waiting_days"`
	TopicRetentionDays   int                        `json:"topic_retention_days"`
}

func Load(path string) (Config, error) {
	c := Config{ArchiveDir: "data/archive", BackupDir: "data/backups", HealthAddress: "127.0.0.1:8080", StartHour: 9, EndHour: 18, FirstResponseMinutes: 30, IdleHours: 4, WaitingDays: 2, TopicRetentionDays: 30}
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	var raw map[string]json.RawMessage
	if e = json.NewDecoder(f).Decode(&raw); e != nil {
		return c, fmt.Errorf("config: %w", e)
	}
	// Older installations may still have removed routing settings. Discard
	// only those known obsolete keys; other unknown settings remain errors.
	delete(raw, "dispatcher_chat_id")
	if value, ok := raw["departments"]; ok {
		var teams map[string]map[string]json.RawMessage
		if e = json.Unmarshal(value, &teams); e != nil {
			return c, e
		}
		for _, team := range teams {
			delete(team, "duty_user_id")
		}
		raw["departments"], e = json.Marshal(teams)
		if e != nil {
			return c, e
		}
	}
	encoded, e := json.Marshal(raw)
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(bytes.NewReader(encoded))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, fmt.Errorf("config: %w", e)
	}
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	c.Token = os.Getenv("TELEGRAM_BOT_TOKEN")
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.AdminID <= 0 {
		return c, fmt.Errorf("admin_id must be a Telegram user ID")
	}
	if c.StartHour < 0 || c.EndHour > 24 || c.StartHour >= c.EndHour {
		return c, fmt.Errorf("invalid business hours")
	}
	if c.FirstResponseMinutes <= 0 || c.IdleHours <= 0 || c.WaitingDays <= 0 || c.TopicRetentionDays < 30 {
		return c, fmt.Errorf("invalid monitoring intervals (retention must be >=30 days)")
	}
	for _, s := range c.Staff {
		if s.ID <= 0 {
			return c, fmt.Errorf("invalid staff ID")
		}
		if s.Role != "engineer" && s.Role != "dispatcher" && s.Role != "admin" {
			return c, fmt.Errorf("invalid staff role %q", s.Role)
		}
		if s.Role == "engineer" && len(s.Departments) == 0 {
			return c, fmt.Errorf("engineer %d requires at least one department", s.ID)
		}
		for _, dept := range s.Departments {
			if !dept.Valid() {
				return c, fmt.Errorf("invalid staff department %q", dept)
			}
		}
	}
	if strings.TrimSpace(c.ArchiveDir) == "" || strings.TrimSpace(c.BackupDir) == "" {
		return c, fmt.Errorf("archive and backup directories required")
	}
	c.ArchiveDir, e = filepath.Abs(c.ArchiveDir)
	if e != nil {
		return c, e
	}
	c.BackupDir, e = filepath.Abs(c.BackupDir)
	if c.ArchiveDir == c.BackupDir {
		return c, fmt.Errorf("archive and backup directories must differ")
	}
	for _, day := range c.Holidays {
		if _, err := time.Parse("2006-01-02", day); err != nil {
			return c, fmt.Errorf("invalid holiday date %q", day)
		}
	}
	return c, e
}
func (c Config) ValidateTelegram() error {
	if c.Token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	seen := map[int64]bool{}
	for _, dept := range []domain.Department{domain.IT, domain.OneC, domain.Installation, domain.Field} {
		t, ok := c.Departments[dept]
		if !ok || t.ChatID == 0 || t.QueueTopicID <= 0 {
			return fmt.Errorf("chat and queue topic required for %s", dept)
		}
		if seen[t.ChatID] {
			return fmt.Errorf("each department requires a distinct group")
		}
		seen[t.ChatID] = true
	}
	return nil
}
