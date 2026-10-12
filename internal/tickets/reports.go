package tickets

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"bot-bvk/internal/domain"
)

type Metrics struct {
	Total, Open, Unassigned, Closed, Transfers, Reopens      int
	MeanFirstResponse, MeanResolution, MeanWorkingResolution time.Duration
}

// Summarize counts response and resolution durations only when corresponding timestamps exist.
// Working resolution includes client waiting; it must not be reported as engineer labor time.
func Summarize(tickets []domain.Ticket, events []domain.Event, calendar Calendar) Metrics {
	var m Metrics
	var responseCount, closedCount int
	for _, t := range tickets {
		m.Total++
		if t.Status.Active() {
			m.Open++
			if t.AssigneeID == 0 {
				m.Unassigned++
			}
		}
		if t.FirstResponseAt != nil {
			responseCount++
			m.MeanFirstResponse += t.FirstResponseAt.Sub(t.CreatedAt)
		}
		if t.Status == domain.Closed && t.ClosedAt != nil {
			m.Closed++
			closedCount++
			m.MeanResolution += t.ClosedAt.Sub(t.CreatedAt)
			m.MeanWorkingResolution += calendar.WorkDuration(t.CreatedAt, *t.ClosedAt)
		}
	}
	for _, e := range events {
		switch e.Kind {
		case "transferred":
			m.Transfers++
		case "reopened":
			m.Reopens++
		}
	}
	if responseCount > 0 {
		m.MeanFirstResponse /= time.Duration(responseCount)
	}
	if closedCount > 0 {
		m.MeanResolution /= time.Duration(closedCount)
		m.MeanWorkingResolution /= time.Duration(closedCount)
	}
	return m
}
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
