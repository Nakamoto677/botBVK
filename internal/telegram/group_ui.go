package telegram

import (
	"fmt"
	"strings"

	"bot-bvk/internal/domain"
)

const groupEmptyHint = "Добавьте к тегу описание проблемы или вложение."

// groupTagList returns the first (canonical) alias of every department for the
// hint text. The parser accepts every configured alias; the hint stays short.
func (a *Adapter) groupTagList() []string {
	tags := make([]string, 0, len(a.cfg.Departments))
	for _, department := range a.configuredDepartments() {
		team := a.cfg.Departments[department]
		if len(team.Tags) > 0 {
			tags = append(tags, team.Tags[0])
			continue
		}
		if defaults := domain.DefaultDepartmentTags(department); len(defaults) > 0 {
			tags = append(tags, defaults[0])
		}
	}
	if len(tags) == 0 {
		return []string{"#it"}
	}
	return tags
}

func (a *Adapter) groupStartHint() string {
	tags := a.groupTagList()
	example := tags[0]
	return fmt.Sprintf("Чтобы создать заявку, добавьте тег отдела: %s. Например: %s Не открывается 1С.", strings.Join(tags, ", "), example)
}

func (a *Adapter) groupAmbiguousHint() string {
	return fmt.Sprintf("В сообщении несколько отделов. Оставьте один тег: %s.", strings.Join(a.groupTagList(), ", "))
}

// groupChatURL builds the only client-chat link the engineer card needs. A
// configured link wins; for a supergroup the trigger message is the anchor.
// A basic group has no t.me/c form, so it uses the invite link that startup
// exports (or the operator's own link) and stays without a jump link only when
// Telegram refused to export one.
func groupChatURL(group ClientGroup, chatID, messageID int64) string {
	if link := strings.TrimSpace(group.Link); link != "" {
		return link
	}
	chat := fmt.Sprint(chatID)
	if messageID > 0 && strings.HasPrefix(chat, "-100") {
		return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(chat, "-100"), messageID)
	}
	return ""
}
