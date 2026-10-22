package config

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"bot-bvk/internal/domain"
)

// ClientGroup maps one company chat to a company and optionally supplies the
// URL used by the engineer card. A -100 supergroup can derive its link from the
// trigger message; a basic group must provide one explicitly.
type ClientGroup struct {
	ChatID  int64  `json:"chat_id"`
	Company string `json:"company"`
	Link    string `json:"link,omitempty"`
}

func normalizeDepartmentTags(departments map[domain.Department]Team) error {
	seen := map[string]domain.Department{}
	urgent := map[string]bool{}
	for _, tag := range domain.UrgencyTags() {
		urgent[tag] = true
	}
	for _, department := range domain.Departments() {
		team, ok := departments[department]
		if !ok {
			continue
		}
		tags := team.Tags
		if len(tags) == 0 {
			tags = domain.DefaultDepartmentTags(department)
		}
		normalized := make([]string, 0, len(tags))
		inside := map[string]bool{}
		for _, raw := range tags {
			tag, err := normalizeTag(raw)
			if err != nil {
				return fmt.Errorf("department %s: %w", department, err)
			}
			if urgent[tag] {
				return fmt.Errorf("department %s: %q is reserved for urgency", department, tag)
			}
			if inside[tag] {
				return fmt.Errorf("department %s: duplicate tag %q", department, tag)
			}
			if owner, found := seen[tag]; found {
				return fmt.Errorf("tag %q is used by both %s and %s", tag, owner, department)
			}
			inside[tag] = true
			seen[tag] = department
			normalized = append(normalized, tag)
		}
		if len(normalized) == 0 {
			return fmt.Errorf("department %s requires at least one tag", department)
		}
		team.Tags = normalized
		departments[department] = team
	}
	return nil
}

func normalizeTag(raw string) (string, error) {
	tag := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(tag, "#") || utf8.RuneCountInString(tag) < 2 || utf8.RuneCountInString(tag) > 64 {
		return "", fmt.Errorf("invalid tag %q", raw)
	}
	for _, r := range tag[1:] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return "", fmt.Errorf("invalid tag %q", raw)
		}
	}
	return tag, nil
}

func validateClientGroups(groups []ClientGroup, departments map[domain.Department]Team) error {
	departmentChats := map[int64]bool{}
	for _, team := range departments {
		if team.ChatID != 0 {
			departmentChats[team.ChatID] = true
		}
	}
	seen := map[int64]bool{}
	for index, group := range groups {
		if group.ChatID >= 0 {
			return fmt.Errorf("client_groups[%d].chat_id=%d must be a negative Telegram chat ID", index, group.ChatID)
		}
		if seen[group.ChatID] {
			return fmt.Errorf("duplicate client group chat_id %d", group.ChatID)
		}
		if departmentChats[group.ChatID] {
			return fmt.Errorf("client group %d cannot be a department group", group.ChatID)
		}
		if strings.TrimSpace(group.Company) == "" {
			return fmt.Errorf("client group %d requires a company", group.ChatID)
		}
		if group.Link != "" && !strings.HasPrefix(group.Link, "https://t.me/") {
			return fmt.Errorf("client group %d link must start with https://t.me/", group.ChatID)
		}
		seen[group.ChatID] = true
	}
	return nil
}
