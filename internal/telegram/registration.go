package telegram

import (
	"context"
	"fmt"
)

// Callback data retains the database ID so old buttons keep their identity.
func (a *Adapter) registrationCompanies(ctx context.Context, m *Message, actor int64, stale bool) error {
	companies, err := a.store.Companies(ctx)
	if err != nil {
		return err
	}
	text := "BVK Support помогает обратиться в техподдержку.\n\nВыберите вашу компанию. Администратор подтвердит доступ, после этого вы сможете создать заявку."
	if stale {
		text = "Эта компания больше недоступна для регистрации. Выберите компанию из актуального списка."
	}
	buttons := [][]Button{}
	for _, c := range companies {
		buttons = append(buttons, []Button{{Text: c.Name, Data: fmt.Sprintf("company:%d", c.ID)}})
	}
	if len(companies) == 0 {
		text = "Сейчас нет компаний для регистрации. Обратитесь к администратору и попробуйте /start позже."
	}
	return a.sayAux(ctx, m, actor, "registration", text, buttons)
}
