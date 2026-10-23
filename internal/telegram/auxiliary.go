package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	"bot-bvk/internal/domain"
)

const actionWorkflow = "active_action"

func actionScenario(ticket int64) string { return "action:" + strconv.FormatInt(ticket, 10) }
func (a *Adapter) sayAux(ctx context.Context, m *Message, actor int64, scenario, text string, buttons [][]Button) error {
	if a.activeUpdate != 0 {
		return a.queueScreen(ctx, m, actor, scenario, text, Markup{buttons})
	}
	sent, err := a.client.Send(ctx, m.Chat.ID, m.ThreadID, text, buttons)
	if err != nil {
		return err
	}
	return a.recordAux(ctx, m, actor, clientAuxScenario(m, scenario, text), sent.ID)
}
func (a *Adapter) recordAux(ctx context.Context, m *Message, actor int64, scenario string, id int64) error {
	if id <= 0 {
		return nil
	}
	aux := domain.AuxiliaryMessage{UserID: actor, ChatID: m.Chat.ID, TopicID: m.ThreadID, Scenario: scenario, MessageID: id}
	if store, ok := a.store.(domain.WorkflowStore); ok {
		return store.SaveAuxiliary(ctx, aux)
	}
	key := auxKey{actor, m.Chat.ID, m.ThreadID, scenario}
	a.auxiliary[key] = append(a.auxiliary[key], aux)
	return nil
}

type auxKey struct {
	UserID, ChatID, TopicID int64
	Scenario                string
}

func (a *Adapter) cleanupAux(ctx context.Context, key promptKey, scenario string) error {
	if store, ok := a.store.(domain.WorkflowStore); ok {
		return store.QueueAuxiliaryCleanup(ctx, key.UserID, key.ChatID, key.TopicID, scenario)
	}
	auxKey := auxKey{key.UserID, key.ChatID, key.TopicID, scenario}
	messages := a.auxiliary[auxKey]
	if len(messages) == 0 {
		return nil
	}
	err := a.store.Transact(ctx, func(tx domain.Tx) error {
		for _, m := range messages {
			raw, _ := json.Marshal(map[string]int64{"chat_id": m.ChatID, "message_id": m.MessageID})
			if e := tx.Enqueue(ctx, domain.Operation{Kind: "delete_aux_message", Payload: raw, AvailableAt: time.Now().UTC(), State: "pending"}); e != nil {
				return e
			}
		}
		return nil
	})
	if err == nil {
		delete(a.auxiliary, auxKey)
	}
	return err
}
func (a *Adapter) cleanupBestEffort(ctx context.Context, key promptKey, scenario string) {
	if err := a.cleanupAux(ctx, key, scenario); err != nil {
		log.Printf("auxiliary cleanup deferred: %v", err)
	}
}
func (a *Adapter) savePrompt(ctx context.Context, key promptKey, p prompt) error {
	if store, ok := a.store.(domain.WorkflowStore); ok {
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if err = store.SaveWorkflow(ctx, key.UserID, key.ChatID, key.TopicID, actionWorkflow, raw); err != nil {
			return err
		}
	}
	a.prompts[key] = p
	return nil
}
func (a *Adapter) loadPrompt(ctx context.Context, key promptKey) (prompt, bool, error) {
	if store, ok := a.store.(domain.WorkflowStore); ok {
		raw, err := store.Workflow(ctx, key.UserID, key.ChatID, key.TopicID, actionWorkflow)
		if errors.Is(err, domain.ErrNotFound) {
			return prompt{}, false, nil
		}
		if err != nil {
			return prompt{}, false, err
		}
		var p prompt
		if err = json.Unmarshal(raw, &p); err != nil {
			return prompt{}, false, err
		}
		return p, true, nil
	}
	p, ok := a.prompts[key]
	return p, ok, nil
}
func (a *Adapter) finishPrompt(ctx context.Context, key promptKey, p prompt) {
	scenario := actionScenario(p.TicketID)
	if p.Action == "company_code" || p.Action == "company_confirm" {
		scenario = companyAccessScenario
	}
	if err := a.cleanupAux(ctx, key, scenario); err != nil {
		log.Printf("action UI cleanup deferred: %v", err)
		return
	}
	if store, ok := a.store.(domain.WorkflowStore); ok {
		current, exists, err := a.loadPrompt(ctx, key)
		if err != nil {
			return
		}
		if exists && current.TicketID == p.TicketID && current.Action == p.Action {
			if err = store.DeleteWorkflow(ctx, key.UserID, key.ChatID, key.TopicID, actionWorkflow); err != nil {
				log.Printf("workflow removal deferred: %v", err)
				return
			}
		}
	}
	if current, ok := a.prompts[key]; ok && current.TicketID == p.TicketID {
		delete(a.prompts, key)
	}
}
func (a *Adapter) replacePrompt(ctx context.Context, key promptKey, p prompt) error {
	old, exists, err := a.loadPrompt(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		a.finishPrompt(ctx, key, old)
	}
	return a.savePrompt(ctx, key, p)
}
