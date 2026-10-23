package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

const companyAccessScenario = "company_access"

type companyCodeService interface {
	CompanyForCode(context.Context, int64, string) (domain.Company, error)
	CompanyCodeVersion(int64) string
	BindCompany(context.Context, int64, string, int64) error
}

func (a *Adapter) companyAccessService() (companyCodeService, error) {
	s, ok := a.service.(companyCodeService)
	if !ok {
		return nil, domain.ErrInvalid
	}
	return s, nil
}

func (a *Adapter) registrationIntro(ctx context.Context, m *Message, actor int64) error {
	companies, err := a.store.Companies(ctx)
	if err != nil {
		return err
	}
	if len(companies) == 0 {
		return a.sayAux(ctx, m, actor, companyAccessScenario, "Сейчас подключение новых компаний недоступно. Обратитесь в поддержку.", nil)
	}
	p := prompt{Action: "company_code", ExpiresAt: time.Now().Add(15 * time.Minute)}
	if err = a.replacePrompt(ctx, scope(actor, m), p); err != nil {
		return err
	}
	return a.sayAux(ctx, m, actor, companyAccessScenario, "BVK Support помогает обратиться в техподдержку.\n\nВведите код, который вам выдала компания. Если у вас есть ссылка-приглашение, просто откройте её.", nil)
}

func (a *Adapter) companyScreen(ctx context.Context, m *Message, u domain.User) error {
	if !u.Approved || u.Role != "client" || m.Chat.Type != "private" {
		return domain.ErrForbidden
	}
	name, err := companyName(ctx, a.store, u.CompanyID)
	if err != nil {
		return err
	}
	return a.sayAux(ctx, m, u.ID, companyAccessScenario, "Ваша компания: «"+name+"».\nЧтобы перейти в другую компанию, понадобится её код.", [][]Button{{{Text: "Сменить компанию", Data: "companychange"}}})
}

func (a *Adapter) beginCompanyChange(ctx context.Context, m *Message, actor int64) error {
	u, err := a.store.User(ctx, actor)
	if err != nil {
		return err
	}
	if !u.Approved || u.Role != "client" || m.Chat.Type != "private" || m.Chat.ID != actor {
		return domain.ErrForbidden
	}
	if ticket, e := a.store.ActiveTicket(ctx, actor); e == nil {
		return a.sayAux(ctx, m, actor, companyAccessScenario, fmt.Sprintf("Сначала завершите заявку №%d. После этого снова откройте «Компания» и введите новый код.", ticket.ID), [][]Button{{{Text: "Открыть заявку", Data: fmt.Sprintf("ticket:%d", ticket.ID)}}})
	} else if !errors.Is(e, domain.ErrNotFound) {
		return e
	}
	p := prompt{Action: "company_code", ExpiresAt: time.Now().Add(15 * time.Minute)}
	if err = a.replacePrompt(ctx, scope(actor, m), p); err != nil {
		return err
	}
	return a.sayAux(ctx, m, actor, companyAccessScenario, "Введите код новой компании. Текущая привязка сохранится, пока вы не подтвердите переход.", nil)
}

func (a *Adapter) handleCompanyCode(ctx context.Context, m *Message, input string) error {
	if m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return domain.ErrForbidden
	}
	a.sensitiveUpdate = true
	s, err := a.companyAccessService()
	if err != nil {
		return err
	}
	company, err := s.CompanyForCode(ctx, m.From.ID, input)
	if errors.Is(err, domain.ErrRateLimited) {
		return a.sayAux(ctx, m, m.From.ID, companyAccessScenario, "Слишком много попыток. Попробуйте ввести код через 15 минут.", nil)
	}
	if errors.Is(err, domain.ErrInvalid) {
		return a.sayAux(ctx, m, m.From.ID, companyAccessScenario, "Код не подошёл. Проверьте его или попросите действующий код у вашей компании.", nil)
	}
	if err != nil {
		return err
	}
	u, err := a.store.User(ctx, m.From.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err == nil && u.Role != "client" {
		return domain.ErrForbidden
	}
	if err == nil && u.Approved && u.CompanyID == company.ID {
		return a.sayAux(ctx, m, m.From.ID, companyAccessScenario, "Вы уже подключены к компании «"+company.Name+"».", nil)
	}
	if ticket, e := a.store.ActiveTicket(ctx, m.From.ID); e == nil {
		return a.sayAux(ctx, m, m.From.ID, companyAccessScenario, fmt.Sprintf("Код компании «%s» верный, но сначала завершите заявку №%d. После этого введите код ещё раз.", company.Name, ticket.ID), nil)
	} else if !errors.Is(e, domain.ErrNotFound) {
		return e
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	version := s.CompanyCodeVersion(company.ID)
	if version == "" {
		return domain.ErrInvalid
	}
	p := prompt{TicketID: company.ID, Action: "company_confirm", Phase: version, FlowID: base64.RawURLEncoding.EncodeToString(nonce[:]), ExpiresAt: time.Now().Add(15 * time.Minute)}
	body := "Подключиться к компании «" + company.Name + "»?"
	if u.Approved {
		body = "Перейти в компанию «" + company.Name + "»? Новые заявки будут относиться к ней."
		if _, exists, e := a.getDraft(ctx, u.ID); e != nil {
			return e
		} else if exists {
			body += "\n\nНезавершённая заявка будет удалена после подтверждения."
		}
	}
	if err = a.replacePrompt(ctx, scope(m.From.ID, m), p); err != nil {
		return err
	}
	return a.sayAux(ctx, m, m.From.ID, companyAccessScenario, body, [][]Button{
		{{Text: "Подтвердить", Data: fmt.Sprintf("bindcompany:%d:%s", company.ID, p.FlowID)}},
		{{Text: "Отмена", Data: fmt.Sprintf("cancelcompany:%d:%s", company.ID, p.FlowID)}},
	})
}

func (a *Adapter) callbackCompanyAccess(ctx context.Context, c *Callback, action string, companyID int64, parts []string) error {
	if c.Message == nil || c.Message.Chat.Type != "private" || c.Message.Chat.ID != c.From.ID {
		return domain.ErrForbidden
	}
	_ = a.client.Answer(ctx, c.ID, "")
	a.markCallbackAnswered()
	m := *c.Message
	m.From = c.From
	if action == "companychange" {
		return a.beginCompanyChange(ctx, &m, c.From.ID)
	}
	key := scope(c.From.ID, &m)
	p, exists, err := a.loadPrompt(ctx, key)
	if err != nil {
		return err
	}
	if !exists || p.Action != "company_confirm" || p.TicketID != companyID || len(parts) != 3 || parts[2] != p.FlowID || time.Now().After(p.ExpiresAt) {
		return a.sayAux(ctx, &m, c.From.ID, companyAccessScenario, "Это подтверждение устарело. Введите код компании ещё раз.", nil)
	}
	if action == "cancelcompany" {
		a.finishPrompt(ctx, key, p)
		return a.sayAux(ctx, &m, c.From.ID, companyAccessScenario, "Переход отменён. Текущая компания не изменилась.", nil)
	}
	s, err := a.companyAccessService()
	if err != nil {
		return err
	}
	if p.Phase == "" || p.Phase != s.CompanyCodeVersion(companyID) {
		a.finishPrompt(ctx, key, p)
		return a.sayAux(ctx, &m, c.From.ID, companyAccessScenario, "Код этой компании изменился. Попросите действующий код и введите его заново.", nil)
	}
	name, err := companyName(ctx, a.store, companyID)
	if err != nil {
		return err
	}
	if err = s.BindCompany(ctx, c.From.ID, strings.TrimSpace(c.From.FirstName+" "+c.From.LastName), companyID); errors.Is(err, domain.ErrConflict) {
		a.finishPrompt(ctx, key, p)
		return a.sayAux(ctx, &m, c.From.ID, companyAccessScenario, "Сначала завершите открытую заявку, затем введите код ещё раз.", nil)
	} else if err != nil {
		return err
	}
	u, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	if err = a.updateName(ctx, u, c.From); err != nil {
		return err
	}
	a.finishPrompt(ctx, key, p)
	return a.clientHome(ctx, &m, "Вы подключены к компании «"+name+"». Выберите действие.")
}
