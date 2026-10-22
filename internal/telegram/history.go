package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var irkutsk = time.FixedZone("Asia/Irkutsk", 8*60*60)

type historyPage struct {
	text    string
	buttons [][]Button
}

func publicMessage(m domain.Message) bool {
	return m.Direction == "inbound" || m.Direction == "outbound"
}
func (a *Adapter) historyPage(ctx context.Context, m *Message, u domain.User, t domain.Ticket, page int, anchor int64) error {
	if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
		return domain.ErrForbidden
	}
	messages, err := a.store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	if anchor == 0 {
		for _, v := range messages {
			if v.ID > anchor {
				anchor = v.ID
			}
		}
	}
	events, err := a.store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	company, err := companyName(ctx, a.store, t.CompanyID)
	if err != nil {
		return err
	}
	client, err := clientContact(ctx, a.store, t.ClientID)
	if t.IsGroup() {
		client, err = displayName(ctx, a.store, t.ClientID, "Чат компании")
	}
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Заявка #%d · %s · %s\nКлиент: %s\nОтдел: %s\nВремя: Иркутск (UTC+8)\nИсходное обращение:\n%s", t.ID, company, statusName(t.Status), client, departmentName(t.Department), t.Description)
	if t.IsGroup() {
		requester := strings.TrimSpace(t.RequesterName)
		if requester == "" {
			requester = "Сотрудник компании"
		}
		if username := strings.TrimSpace(t.RequesterUsername); username != "" {
			requester += " · @" + strings.TrimPrefix(username, "@")
		}
		header = fmt.Sprintf("Заявка #%d · %s · %s\nКомпания: %s\nЧат компании: %s\nОбратился: %s\nОтдел: %s\nВремя: Иркутск (UTC+8)\nИсходное обращение:\n%s", t.ID, company, statusName(t.Status), company, client, requester, departmentName(t.Department), t.Description)
	}
	header = strings.Replace(header, "\nВремя: Иркутск", "\nСрочность: "+urgencyLabel(t.Urgency)+"\nВремя: Иркутск", 1)
	resolution := ""
	for _, e := range events {
		if e.Kind == "closed" {
			var v struct {
				Resolution string `json:"resolution"`
			}
			if json.Unmarshal(e.Data, &v) == nil {
				resolution = v.Resolution
			}
		}
	}
	if resolution == "" {
		resolution = t.Resolution
	}
	if resolution != "" {
		header += "\n\nИтог последнего закрытия:\n" + resolution
	}
	pages := []historyPage{}
	appendPage := func(text string, buttons [][]Button) {
		for _, part := range splitText(text, 3500) {
			pages = append(pages, historyPage{part, buttons})
		}
	}
	headerButtons := clientTicketButtons(t)
	if u.Role != "client" {
		headerButtons = [][]Button{{{Text: "История", Data: fmt.Sprintf("history:%d", t.ID)}}}
		team := a.cfg.Departments[t.Department]
		target := t.TopicMessageID
		if target == 0 {
			target = t.QueueMessageID
		}
		if strings.HasPrefix(fmt.Sprint(t.TopicChatID), "-100") && target > 0 && t.TopicMessageID > 0 {
			headerButtons = append(headerButtons, []Button{{Text: "Открыть карточку в топике", URL: fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(fmt.Sprint(t.TopicChatID), "-100"), target)}})
		} else if strings.HasPrefix(fmt.Sprint(team.ChatID), "-100") && target > 0 {
			headerButtons = append(headerButtons, []Button{{Text: "Открыть карточку в очереди", URL: fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(fmt.Sprint(team.ChatID), "-100"), target)}})
		}
		if t.IsGroup() {
			if group, ok := a.cfg.ClientGroups[t.ClientID]; ok {
				if link := groupChatURL(group, t.ClientID, t.SourceMessageID); link != "" {
					headerButtons = append(headerButtons, []Button{{Text: "В чат компании", URL: link}})
				}
			}
		}
	}
	appendPage(header, headerButtons)
	for _, v := range messages {
		if v.ID > anchor || u.Role == "client" && !publicMessage(v) {
			continue
		}
		author, e := displayName(ctx, a.store, v.AuthorID, "Участник")
		if e != nil {
			return e
		}
		side := map[string]string{"inbound": "Клиент", "outbound": "Специалист", "service": "Служебное", "internal": "Внутреннее"}[v.Direction]
		text := fmt.Sprintf("%s · %s · %s\n%s", v.CreatedAt.In(irkutsk).Format("02.01.2006 15:04"), side, author, v.Text)
		buttons := [][]Button{}
		if v.FileID != "" {
			state := "независимая копия отсутствует"
			if v.ArchivePath != "" && v.SHA256 != "" {
				state = "копия в архиве; проверяется при выдаче"
			}
			text += fmt.Sprintf("\nВложение #%d: %s (%s), %s", v.ID, v.FileName, v.MediaType, state)
			buttons = append(buttons, []Button{{Text: fmt.Sprintf("Получить вложение #%d", v.ID), Data: fmt.Sprintf("historyfile:%d:%d", t.ID, v.ID)}})
		}
		for _, r := range v.Revisions {
			text += fmt.Sprintf("\nПредыдущая версия (%s): %s", r.EditedAt.In(irkutsk).Format("02.01.2006 15:04"), r.Text)
		}
		appendPage(text, buttons)
	}
	for _, e := range events {
		if e.Kind == "topic_closing_notice" {
			var v struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Data, &v) == nil && v.Text != "" {
				appendPage(fmt.Sprintf("%s · Служебное\n%s", e.CreatedAt.In(irkutsk).Format("02.01.2006 15:04"), v.Text), nil)
			}
		}
	}
	if u.Role != "client" {
		for _, e := range events {
			if e.Kind == "transferred" {
				var v struct {
					From   domain.Department `json:"from"`
					To     domain.Department `json:"to"`
					Reason string            `json:"reason"`
				}
				if json.Unmarshal(e.Data, &v) == nil {
					appendPage(fmt.Sprintf("%s · Передача: %s → %s\n%s", e.CreatedAt.In(irkutsk).Format("02.01.2006 15:04"), departmentName(v.From), departmentName(v.To), transferReason(v.Reason)), nil)
				}
			}
		}
	}
	if page < 0 || page >= len(pages) {
		return domain.ErrInvalid
	}
	p := pages[page]
	p.text = fmt.Sprintf("История #%d · страница %d/%d\n\n", t.ID, page+1, len(pages)) + p.text
	nav := []Button{}
	if page > 0 {
		nav = append(nav, Button{Text: "Назад", Data: fmt.Sprintf("history:%d:%d:%d", t.ID, page-1, anchor)})
	}
	if page+1 < len(pages) {
		nav = append(nav, Button{Text: "Далее", Data: fmt.Sprintf("history:%d:%d:%d", t.ID, page+1, anchor)})
	}
	if len(nav) > 0 {
		p.buttons = append(p.buttons, nav)
	}
	return a.say(ctx, m, p.text, p.buttons)
}
func (a *Adapter) exactTicket(ctx context.Context, m *Message, u domain.User) error {
	fields := strings.Fields(m.Text)
	if len(fields) != 2 {
		return domain.ErrInvalid
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || id <= 0 {
		return domain.ErrInvalid
	}
	t, err := a.store.Ticket(ctx, id)
	if err != nil {
		return err
	}
	return a.showTicket(ctx, m, u, t)
}
func (a *Adapter) ticketListPage(ctx context.Context, m *Message, u domain.User, page int, anchor int64) error {
	if !u.Approved || page < 0 {
		return domain.ErrForbidden
	}
	tickets, err := a.store.Tickets(ctx)
	if err != nil {
		return err
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].ID > tickets[j].ID })
	if anchor == 0 && len(tickets) > 0 {
		anchor = tickets[0].ID
	}
	visible := []domain.Ticket{}
	for _, t := range tickets {
		if t.ID <= anchor && historyAuthorized(u, t) && a.historyDestination(m, u, t) {
			visible = append(visible, t)
		}
	}
	start := page * 10
	if start > len(visible) {
		return domain.ErrInvalid
	}
	end := min(start+10, len(visible))
	text := fmt.Sprintf("Ваши заявки · страница %d\n", page+1)
	buttons := [][]Button{}
	for _, t := range visible[start:end] {
		text += fmt.Sprintf("#%d · %s · %s\n", t.ID, departmentName(t.Department), statusName(t.Status))
		buttons = append(buttons, []Button{{Text: fmt.Sprintf("#%d · %s", t.ID, statusName(t.Status)), Data: fmt.Sprintf("ticket:%d", t.ID)}})
	}
	if len(visible) == 0 {
		text += "Заявок нет."
	}
	nav := []Button{}
	if page > 0 {
		nav = append(nav, Button{Text: "Назад", Data: fmt.Sprintf("tickets:%d:%d", page-1, anchor)})
	}
	if end < len(visible) {
		nav = append(nav, Button{Text: "Далее", Data: fmt.Sprintf("tickets:%d:%d", page+1, anchor)})
	}
	if len(nav) > 0 {
		buttons = append(buttons, nav)
	}
	return a.say(ctx, m, text, buttons)
}
func (a *Adapter) historyFile(ctx context.Context, m *Message, u domain.User, t domain.Ticket, id int64) error {
	if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
		return domain.ErrForbidden
	}
	messages, err := a.store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, v := range messages {
		if v.ID != id {
			continue
		}
		if u.Role == "client" && !publicMessage(v) {
			return domain.ErrForbidden
		}
		if v.FileID == "" {
			return domain.ErrNotFound
		}
		if path, ok := verifiedHistoryFile(a.cfg.ArchiveDir, v); ok {
			return a.client.SendLocalFile(ctx, m.Chat.ID, m.ThreadID, path, v.FileName)
		}
		if err = a.say(ctx, m, "Проверенной независимой копии нет. Пробую получить вложение из Telegram.", nil); err != nil {
			return err
		}
		w := NewWorker(a.client, a.store, a.cfg)
		w.interval = 0
		// A user request is a fresh send, independent of its original delivery.
		v.ID = 0
		v.Text = ""
		v.TelegramMessageID = a.activeUpdate
		if v.TelegramMessageID == 0 {
			v.TelegramMessageID = time.Now().UnixNano()
		}
		v.TelegramChatID = m.Chat.ID
		_, err = w.deliverNamed(ctx, m.Chat.ID, m.ThreadID, v, "")
		if err != nil {
			return a.say(ctx, m, "Вложение недоступно: проверенной локальной копии и доступного файла Telegram нет.", nil)
		}
		return nil
	}
	return domain.ErrNotFound
}
func verifiedHistoryFile(root string, m domain.Message) (string, bool) {
	if root == "" || m.ArchivePath == "" || m.SHA256 == "" {
		return "", false
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	path, err := filepath.EvalSymlinks(m.ArchivePath)
	if err != nil {
		return "", false
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// The path comes exclusively from the saved message, never callback data.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	hash := sha256.Sum256(data)
	return path, hex.EncodeToString(hash[:]) == m.SHA256
}
