package report

import (
	"bytes"
	"context"
	"encoding/csv"
	"strconv"
	"strings"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
)

func CSV(ctx context.Context, store domain.Store, calendar tickets.Calendar) (string, error) {
	all, err := store.Tickets(ctx)
	if err != nil {
		return "", err
	}
	var base bytes.Buffer
	if err = tickets.WriteCSV(&base, all, calendar); err != nil {
		return "", err
	}
	rows, err := csv.NewReader(&base).ReadAll()
	if err != nil {
		return "", err
	}
	rows[0] = append(rows[0], "first_response_calendar_seconds", "first_response_working_seconds", "waiting_calendar_seconds", "waiting_working_seconds", "transfers", "reopens")
	for i, t := range all {
		events, err := store.Events(ctx, t.ID)
		if err != nil {
			return "", err
		}
		until := time.Now().UTC()
		if t.ClosedAt != nil {
			until = *t.ClosedAt
		}
		wait, workWait := tickets.WaitingDuration(t.ID, events, until, calendar)
		response, workResponse := "", ""
		if t.FirstResponseAt != nil {
			response = seconds(t.FirstResponseAt.Sub(t.CreatedAt))
			workResponse = seconds(calendar.WorkDuration(t.CreatedAt, *t.FirstResponseAt))
		}
		transfers, reopens := 0, 0
		for _, ev := range events {
			if ev.Kind == "transferred" {
				transfers++
			}
			if ev.Kind == "reopened" {
				reopens++
			}
		}
		rows[i+1] = append(rows[i+1], response, workResponse, seconds(wait), seconds(workWait), strconv.Itoa(transfers), strconv.Itoa(reopens))
	}
	var out strings.Builder
	w := csv.NewWriter(&out)
	if err = w.WriteAll(rows); err != nil {
		return "", err
	}
	return out.String(), nil
}
func seconds(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) }
