package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
)

type transferContext struct {
	From      domain.Department `json:"from"`
	To        domain.Department `json:"to"`
	Reason    string            `json:"reason"`
	MessageID int64             `json:"message_id"`
	EventID   int64             `json:"-"`
}

func transferReason(s string) string {
	if strings.TrimSpace(s) == "-" {
		return "Без комментария"
	}
	return s
}
func (w *Worker) transferContext(ctx context.Context, t domain.Ticket) (transferContext, bool, error) {
	events, err := w.store.Events(ctx, t.ID)
	if err != nil {
		return transferContext{}, false, err
	}
	var latest transferContext
	exists := false
	for _, e := range events {
		if e.Kind == "reopened" {
			exists = false
			continue
		}
		if e.Kind == "transferred" {
			var v transferContext
			if err = json.Unmarshal(e.Data, &v); err != nil {
				return v, false, err
			}
			v.EventID = e.ID
			latest = v
			exists = v.To == t.Department
		}
	}
	return latest, exists, nil
}
func (w *Worker) ensureTransferContext(ctx context.Context, t domain.Ticket) error {
	v, exists, err := w.transferContext(ctx, t)
	if err != nil || !exists {
		return err
	}
	text := fmt.Sprintf("Заявка передана из отдела %s.\n%s", departmentName(v.From), transferReason(v.Reason))
	buttons := [][]Button{{{Text: "История заявки", Data: fmt.Sprintf("history:%d", t.ID)}}}
	if v.MessageID > 0 {
		buttons = append(buttons, []Button{{Text: "Вложение передачи", Data: fmt.Sprintf("historyfile:%d:%d", t.ID, v.MessageID)}})
	}
	_, err = w.textParts(ctx, fmt.Sprintf("transfer:%d:%d:%d:%d", t.ID, v.EventID, t.TopicChatID, t.TopicID), t.TopicChatID, t.TopicID, text, Markup{buttons})
	return err
}
func (w *Worker) queueTransferContext(ctx context.Context, t domain.Ticket) (string, error) {
	v, exists, err := w.transferContext(ctx, t)
	if err != nil || !exists {
		return "", err
	}
	text := "\n\n<b>Передано из:</b> " + departmentName(v.From) + "\n" + html.EscapeString(previewText(transferReason(v.Reason), 600))
	if v.MessageID > 0 {
		text += "\nСлужебное вложение доступно в /history " + fmt.Sprint(t.ID)
	}
	return text, nil
}
