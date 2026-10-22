package tickets

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"time"

	"bot-bvk/internal/domain"
)

func WriteCSV(w io.Writer, tickets []domain.Ticket, calendar Calendar) error {
	out := csv.NewWriter(w)
	if err := out.Write([]string{"ticket_id", "company_id", "client_id", "department", "status", "assignee_id", "created_at", "first_response_at", "closed_at", "closed_by", "resolution_calendar_seconds", "resolution_working_seconds", "resolution"}); err != nil {
		return err
	}
	for _, t := range tickets {
		response, closed, elapsed, working := "", "", "", ""
		if t.FirstResponseAt != nil {
			response = t.FirstResponseAt.UTC().Format(time.RFC3339)
		}
		if t.ClosedAt != nil {
			closed = t.ClosedAt.UTC().Format(time.RFC3339)
			elapsed = strconv.FormatInt(int64(t.ClosedAt.Sub(t.CreatedAt)/time.Second), 10)
			working = strconv.FormatInt(int64(calendar.WorkDuration(t.CreatedAt, *t.ClosedAt)/time.Second), 10)
		}
		row := []string{strconv.FormatInt(t.ID, 10), strconv.FormatInt(t.CompanyID, 10), strconv.FormatInt(t.ClientID, 10), string(t.Department), string(t.Status), strconv.FormatInt(t.AssigneeID, 10), t.CreatedAt.UTC().Format(time.RFC3339), response, closed, strconv.FormatInt(t.ClosedBy, 10), elapsed, working, csvSafe(t.Resolution)}
		if err := out.Write(row); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// Prevent spreadsheet formula interpretation for user-supplied exported values.
func csvSafe(s string) string {
	if len(s) > 0 {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + s
		}
	}
	return s
}

// WaitingDuration reconstructs waiting intervals from status events up to until.
func WaitingDuration(ticketID int64, events []domain.Event, until time.Time, calendar Calendar) (time.Duration, time.Duration) {
	selected := make([]domain.Event, 0)
	for _, e := range events {
		if e.TicketID == ticketID && !e.CreatedAt.After(until) {
			selected = append(selected, e)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].CreatedAt.Equal(selected[j].CreatedAt) {
			return selected[i].ID < selected[j].ID
		}
		return selected[i].CreatedAt.Before(selected[j].CreatedAt)
	})
	var start *time.Time
	var total, working time.Duration
	for _, e := range selected {
		waiting := false
		stop := false
		switch e.Kind {
		case "status_changed":
			var payload struct {
				Status domain.Status `json:"status"`
			}
			if json.Unmarshal(e.Data, &payload) != nil {
				continue
			}
			waiting = payload.Status == domain.Waiting
			stop = !waiting
		case "closed", "cancelled", "transferred", "reopened":
			stop = true
		}
		if waiting && start == nil {
			at := e.CreatedAt
			start = &at
		}
		if stop && start != nil {
			total += e.CreatedAt.Sub(*start)
			working += calendar.WorkDuration(*start, e.CreatedAt)
			start = nil
		}
	}
	if start != nil {
		total += until.Sub(*start)
		working += calendar.WorkDuration(*start, until)
	}
	return total, working
}
