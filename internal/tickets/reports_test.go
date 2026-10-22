package tickets

import (
	"bot-bvk/internal/domain"
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestSafeCSV(t *testing.T) {
	c := DefaultCalendar()
	start := time.Date(2026, 10, 9, 17, 0, 0, 0, c.Location)
	response := start.Add(30 * time.Minute)
	end := time.Date(2026, 10, 12, 10, 0, 0, 0, c.Location)
	tickets := []domain.Ticket{{ID: 1, CreatedAt: start, FirstResponseAt: &response, ClosedAt: &end, Status: domain.Closed, Resolution: "=HYPERLINK(\"evil\")"}, {ID: 2, Status: domain.New}}
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

func TestWaitingIntervalsSortedAndClipped(t *testing.T) {
	c := DefaultCalendar()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, c.Location)
	end := start.Add(2 * time.Hour)
	events := []domain.Event{{TicketID: 1, Kind: "closed", CreatedAt: end}, {TicketID: 1, Kind: "status_changed", CreatedAt: start, Data: []byte(`{"status":"waiting_client"}`)}, {TicketID: 2, Kind: "status_changed", CreatedAt: start, Data: []byte(`{"status":"waiting_client"}`)}}
	actual, working := WaitingDuration(1, events, end, c)
	if actual != 2*time.Hour || working != actual {
		t.Fatal(actual, working)
	}
}
