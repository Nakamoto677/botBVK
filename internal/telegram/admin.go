package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"bot-bvk/internal/domain"
)

func (a *Adapter) admin(ctx context.Context, m *Message, u domain.User) error {
	if !u.Approved || u.Role != "admin" || m.Chat.Type != "private" {
		return domain.ErrForbidden
	}
	fields := strings.Fields(m.Text)
	if len(fields) == 0 {
		return domain.ErrInvalid
	}
	switch fields[0] {
	case "/requests":
		requests, err := a.store.AccessRequests(ctx)
		if err != nil {
			return err
		}
		for _, r := range requests {
			if r.Status != "pending" {
				continue
			}
			if err = a.say(ctx, m, fmt.Sprintf("Запрос #%d · аккаунт %d · компания %d", r.ID, r.UserID, r.CompanyID), [][]Button{{{Text: "Одобрить", Data: fmt.Sprintf("approve:%d", r.ID)}, {Text: "Отклонить", Data: fmt.Sprintf("reject:%d", r.ID)}}}); err != nil {
				return err
			}
		}
		return nil
	case "/revoke", "/staff":
		if len(fields) < 2 {
			return domain.ErrInvalid
		}
		id, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || id <= 0 {
			return domain.ErrInvalid
		}
		for _, configured := range a.cfg.AdminIDs {
			if configured == id {
				return a.say(ctx, m, "Измените список базовых администраторов в конфигурации. Их доступ защищён.", nil)
			}
		}
		if fields[0] == "/staff" && (len(fields) < 3 || (fields[2] != "engineer" && fields[2] != "dispatcher" && fields[2] != "admin")) {
			return domain.ErrInvalid
		}
		departments := []domain.Department{}
		if fields[0] == "/staff" {
			for _, field := range fields[3:] {
				d := domain.Department(field)
				if !d.Valid() {
					return domain.ErrInvalid
				}
				departments = append(departments, d)
			}
			if fields[2] == "engineer" && len(departments) == 0 {
				return domain.ErrInvalid
			}
		}
		err = a.store.Transact(ctx, func(tx domain.Tx) error {
			actor, e := tx.GetUser(ctx, u.ID)
			if e != nil {
				return e
			}
			if !actor.Approved || actor.Role != "admin" {
				return domain.ErrForbidden
			}
			user, e := tx.GetUser(ctx, id)
			if e != nil && !errors.Is(e, domain.ErrNotFound) {
				return e
			}
			if errors.Is(e, domain.ErrNotFound) {
				if fields[0] == "/revoke" {
					return e
				}
				user = domain.User{ID: id, Name: fmt.Sprintf("Сотрудник %d", id)}
			}
			if fields[0] == "/revoke" {
				user.Approved = false
			} else {
				user.Role = fields[2]
				user.Approved = true
				user.Departments = departments
				user.CompanyID = 0
			}
			return tx.SaveUser(ctx, user)
		})
		if err != nil {
			return err
		}
		return a.say(ctx, m, "Доступ обновлён.", nil)
	default:
		return domain.ErrInvalid
	}
}
