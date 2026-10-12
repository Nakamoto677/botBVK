package tickets

import (
	"bot-bvk/internal/domain"
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestMetricsAndSafeCSV(t *testing.T) {
	c := DefaultCalendar()
	start := time.Date(2026, 10, 9, 17, 0, 0, 0, c.Location)
	response := start.Add(30 * time.Minute)
	end := time.Date(2026, 10, 12, 10, 0, 0, 0, c.Location)
	tickets := []domain.Ticket{{ID: 1, CreatedAt: start, FirstResponseAt: &response, ClosedAt: &end, Status: domain.Closed, Resolution: "=HYPERLINK(\"evil\")"}, {ID: 2, Status: domain.New}}
	m := Summarize(tickets, []domain.Event{{Kind: "transferred"}, {Kind: "reopened"}}, c)
	if m.Total != 2 || m.Open != 1 || m.Unassigned != 1 || m.Closed != 1 || m.Transfers != 1 || m.Reopens != 1 || m.MeanWorkingResolution != 2*time.Hour || m.MeanFirstResponse != 30*time.Minute {
		t.Fatal(m)
	}
	var b bytes.Buffer
	if err := WriteCSV(&b, tickets, c); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rows[1][12], "'=") {
		t.Fatal(rows[1][12])
	}
}
func TestReopenUsesExistingTopic(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	v := f.tickets[ticket.ID]
	v.TopicID = 42
	v.TopicChatID = -100
	f.tickets[v.ID] = v
	_ = s.Close(t.Context(), 2, v.ID, "fixed")
	if err := s.Reopen(t.Context(), 1, v.ID); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-2].Kind != "reopen_topic" || f.tickets[v.ID].TopicID != 42 {
		t.Fatal("existing topic was replaced")
	}
}
