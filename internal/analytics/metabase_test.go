package analytics

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

var optionalSQL = regexp.MustCompile(`(?s)\[\[(.*?)\]\]`)

func renderSQL(sql string, filtered bool) string {
	sql = optionalSQL.ReplaceAllStringFunc(sql, func(s string) string {
		if !filtered {
			return ""
		}
		return s[2 : len(s)-2]
	})
	values := map[string]string{"start_date": "'2026-10-12'", "end_date": "'2026-10-13'", "row_start": "'2026-10-12'", "row_end": "'2026-10-13'", "department": "'it'", "employee": "20", "company": "1", "ticket_id": "1"}
	for name, value := range values {
		sql = strings.ReplaceAll(sql, "{{"+name+"}}", value)
	}
	return sql
}

// Keep only the period filters when comparing the overview with its drilldowns.
func renderWindowSQL(sql, start, end string) string {
	sql = optionalSQL.ReplaceAllStringFunc(sql, func(s string) string {
		if strings.Contains(s, "{{start_date}}") || strings.Contains(s, "{{end_date}}") {
			return s[2 : len(s)-2]
		}
		return ""
	})
	sql = strings.ReplaceAll(sql, "{{start_date}}", "'"+start+"'")
	return strings.ReplaceAll(sql, "{{end_date}}", "'"+end+"'")
}

func renderPeriodSQL(sql string) string {
	return renderWindowSQL(sql, "2026-10-12", "2026-10-13")
}

func metabaseSource() Source {
	src := Source{At: at(13, 16, 0), Companies: []domain.Company{{ID: 1, Name: "Компания"}}, Users: []domain.User{{ID: 100, Name: "Клиент", Role: "client"}, {ID: 10, Name: "Первый", Role: "engineer"}, {ID: 20, Name: "Второй", Role: "engineer"}}}
	src.Tickets = []domain.Ticket{{ID: 1, ClientID: 100, CompanyID: 1, Department: domain.OneC, Status: domain.Closed, AssigneeID: 20, CreatedAt: at(9, 9, 0)}, {ID: 2, ClientID: 100, CompanyID: 1, Department: domain.IT, Status: domain.New, CreatedAt: at(12, 9, 0)}, {ID: 3, ClientID: 100, CompanyID: 1, Department: domain.IT, Status: domain.Cancelled, CreatedAt: at(12, 9, 0)}}
	src.Events = []domain.Event{event(1, "created", at(9, 9, 0), 100, "null"), event(2, "claimed", at(9, 9, 5), 10, "null"), event(3, "message_delivered", at(12, 9, 10), 10, `{"source_message_id":1,"direction":"outbound"}`), event(4, "closed", at(12, 10, 0), 10, `{"resolution":"Первое"}`), event(5, "reopened", at(12, 11, 0), 100, `{"from":"it"}`), event(6, "transferred", at(12, 12, 0), 10, `{"from":"it","to":"1c"}`), event(7, "claimed", at(12, 12, 5), 20, "null"), event(8, "closed", at(13, 13, 0), 20, `{"resolution":"Второе"}`)}
	src.Messages = []domain.Message{{ID: 1, TicketID: 1, AuthorID: 10, Direction: "outbound", CreatedAt: at(12, 9, 5)}}
	ev := event(9, "created", at(12, 9, 0), 100, "null")
	ev.TicketID = 2
	src.Events = append(src.Events, ev)
	ev = event(10, "created", at(12, 9, 0), 100, "null")
	ev.TicketID = 3
	src.Events = append(src.Events, ev)
	ev = event(11, "cancelled", at(12, 10, 0), 100, "null")
	ev.TicketID = 3
	src.Events = append(src.Events, ev)
	return src
}

func TestMetabaseQueriesAndExactDrilldowns(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = publish(ctx, tx, Calculate(metabaseSource(), options())); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../analytics/metabase/sql/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 39 {
		t.Fatal("missing cards", len(files))
	}
	for _, file := range files {
		sql, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		for _, filtered := range []bool{false, true} {
			t.Run(filepath.Base(file)+map[bool]string{false: "_defaults", true: "_filtered"}[filtered], func(t *testing.T) {
				rows, e := s.Pool.Query(ctx, renderSQL(string(sql), filtered))
				if e != nil {
					t.Fatal(e)
				}
				for rows.Next() {
					if _, e = rows.Values(); e != nil {
						t.Fatal(e)
					}
				}
				rows.Close()
				if e = rows.Err(); e != nil {
					t.Fatal(e)
				}
			})
		}
	}
	for _, file := range files {
		if !strings.HasSuffix(file, "_count.sql") {
			continue
		}
		list := strings.TrimSuffix(file, "_count.sql") + "_list.sql"
		a, _ := os.ReadFile(file)
		b, err := os.ReadFile(list)
		if err != nil {
			t.Fatal(err)
		}
		for _, filtered := range []bool{false, true} {
			var n, m int64
			if err = s.Pool.QueryRow(ctx, renderSQL(string(a), filtered)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			q := "SELECT count(*) FROM (" + strings.TrimSuffix(strings.TrimSpace(renderSQL(string(b), filtered)), ";") + ") exact_list"
			if err = s.Pool.QueryRow(ctx, q).Scan(&m); err != nil {
				t.Fatal(err)
			}
			if n != m {
				t.Fatalf("%s count=%d list=%d", file, n, m)
			}
		}
	}
	// Last closure must be selected before staff and department filters.
	sql, _ := os.ReadFile("../../analytics/metabase/sql/closed_count.sql")
	q := renderSQL(string(sql), true)
	q = strings.ReplaceAll(q, "'it'", "'1c'")
	var n int
	if err = s.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("last closure missing", n)
	}
	q = strings.ReplaceAll(q, "= 20", "= 10")
	if err = s.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("first closure leaked after actor filter", n)
	}
	// A response in IT remains in IT statistics after closure in 1C.
	sql, _ = os.ReadFile("../../analytics/metabase/sql/first_response.sql")
	q = renderSQL(string(sql), true)
	q = strings.ReplaceAll(q, "=20", "=10")
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing reply row")
	}
	vals, err := rows.Values()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if vals[0].(int64) != 1 {
		t.Fatal("reply department changed", vals)
	}
	// Queue balance must reconcile events, including transfers, for all/dept.
	sql, _ = os.ReadFile("../../analytics/metabase/sql/queue_balance.sql")
	for _, filtered := range []bool{false, true} {
		rows, err = s.Pool.Query(ctx, renderSQL(string(sql), filtered))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			v, e := rows.Values()
			if e != nil {
				t.Fatal(e)
			}
			if v[6].(int64) != 0 {
				t.Fatal("unbalanced queue", v)
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Empty/reversed periods are valid empty reports.
	sql, _ = os.ReadFile("../../analytics/metabase/sql/received_count.sql")
	q = renderSQL(string(sql), true)
	q = strings.ReplaceAll(q, "'2026-10-12'", "'2026-10-14'")
	if err = s.Pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("empty period", n)
	}
	// The timeline assigns a repeated closure to its last day in the whole period.
	flow, _ := os.ReadFile("../../analytics/metabase/sql/flow.sql")
	rows, err = s.Pool.Query(ctx, renderPeriodSQL(string(flow)))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var received, closed, cancelled int64
	openByDay := map[string]int64{}
	for rows.Next() {
		var label string
		var a, b, c, open int64
		var first, last time.Time
		if err = rows.Scan(&label, &a, &b, &c, &open, &first, &last); err != nil {
			t.Fatal(err)
		}
		if b > 0 && first.Day() != 13 {
			t.Fatalf("closure placed in wrong row: %s", label)
		}
		if !first.Equal(last) {
			t.Fatalf("daily drilldown spans multiple days: %s – %s", first, last)
		}
		received, closed, cancelled = received+a, closed+b, cancelled+c
		openByDay[first.Format("2006-01-02")] = open
	}
	rows.Close()
	if received != 2 || closed != 1 || cancelled != 1 {
		t.Fatalf("timeline totals: %d %d %d", received, closed, cancelled)
	}
	if openByDay["2026-10-12"] != 2 || openByDay["2026-10-13"] != 1 {
		t.Fatalf("historical open queue: %v", openByDay)
	}
	backlog, _ := os.ReadFile("../../analytics/metabase/sql/backlog_list.sql")
	for day, want := range map[string]int64{"2026-10-12": 2, "2026-10-13": 1} {
		var count int64
		query := "SELECT count(*) FROM (" + renderWindowSQL(string(backlog), "2026-10-12", day) + ") cell"
		if err = s.Pool.QueryRow(ctx, query).Scan(&count); err != nil || count != want {
			t.Fatalf("open cell %s: got %d, want %d: %v", day, count, want, err)
		}
	}
	for start, wantRows := range map[string]int64{"2026-09-13": 5, "2026-09-12": 2, "2026-07-16": 2, "2026-07-15": 1} {
		var count int64
		query := "SELECT count(*) FROM (" + renderWindowSQL(string(flow), start, "2026-10-13") + ") grouped"
		if err = s.Pool.QueryRow(ctx, query).Scan(&count); err != nil || count != wantRows {
			t.Fatalf("grouping from %s: got %d, want %d: %v", start, count, wantRows, err)
		}
	}
	var emptyLabel string
	empty := renderWindowSQL(string(flow), "2026-10-14", "2026-10-14")
	if err = s.Pool.QueryRow(ctx, "SELECT \"Дата / период\" FROM ("+empty+") grouped").Scan(&emptyLabel); err != nil || emptyLabel != "За выбранный период нет заявок" {
		t.Fatalf("empty timeline: %q %v", emptyLabel, err)
	}
	for key, want := range map[string]int64{"received": received, "closed": closed, "cancelled": cancelled} {
		raw, _ := os.ReadFile("../../analytics/metabase/sql/" + key + "_count.sql")
		var count int64
		if err = s.Pool.QueryRow(ctx, renderPeriodSQL(string(raw))).Scan(&count); err != nil || count != want {
			t.Fatalf("%s timeline=%d card=%d: %v", key, want, count, err)
		}
	}
	closedList, _ := os.ReadFile("../../analytics/metabase/sql/closed_list.sql")
	for day, want := range map[string]int64{"2026-10-12": 0, "2026-10-13": 1} {
		cellSQL := optionalSQL.ReplaceAllStringFunc(string(closedList), func(s string) string {
			if strings.Contains(s, "{{start_date}}") || strings.Contains(s, "{{end_date}}") || strings.Contains(s, "{{row_start}}") || strings.Contains(s, "{{row_end}}") {
				return s[2 : len(s)-2]
			}
			return ""
		})
		for key, value := range map[string]string{"start_date": "2026-10-12", "end_date": "2026-10-13", "row_start": day, "row_end": day} {
			cellSQL = strings.ReplaceAll(cellSQL, "{{"+key+"}}", "'"+value+"'")
		}
		var count int64
		if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM ("+cellSQL+") cell").Scan(&count); err != nil || count != want {
			t.Fatalf("closed cell %s: got %d, want %d: %v", day, count, want, err)
		}
	}
	team, _ := os.ReadFile("../../analytics/metabase/sql/employees.sql")
	rows, err = s.Pool.Query(ctx, renderPeriodSQL(string(team)))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		values, e := rows.Values()
		if e != nil {
			t.Fatal(e)
		}
		if values[0] == int64(20) {
			found = true
			if values[2] != int64(1) || values[4] != "24 часа 55 мин" || values[5] != "24 часа 55 мин" || values[6] != "24 часа 55 мин" || values[7] != int64(1) {
				t.Fatalf("employee result: %v", values)
			}
		}
	}
	rows.Close()
	if !found {
		t.Fatal("closing employee missing")
	}
}

// A partial current period is compared with the same elapsed duration, not a
// whole previous month. Repeated closures must still count the ticket once.
func TestMetabaseComparisonPartialPeriod(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	src := metabaseSource()
	for i, created := range []struct{ day, hour int }{{10, 9}, {10, 7}, {13, 17}} {
		id := int64(i + 4)
		src.Tickets = append(src.Tickets, domain.Ticket{ID: id, ClientID: 100, CompanyID: 1, Department: domain.IT, Status: domain.New, CreatedAt: at(created.day, created.hour, 0)})
		e := event(id+20, "created", at(created.day, created.hour, 0), 100, "null")
		e.TicketID = id
		src.Events = append(src.Events, e)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = publish(ctx, tx, Calculate(src, options())); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../analytics/metabase/sql/comparison.sql")
	if err != nil {
		t.Fatal(err)
	}
	q := optionalSQL.ReplaceAllStringFunc(string(raw), func(part string) string {
		if strings.Contains(part, "{{start_date}}") || strings.Contains(part, "{{end_date}}") {
			return part
		}
		return ""
	})
	rows, err := s.Pool.Query(ctx, renderSQL(q, true))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][3]int64{}
	for rows.Next() {
		var name string
		var current, previous, delta int64
		if err = rows.Scan(&name, &current, &previous, &delta); err != nil {
			t.Fatal(err)
		}
		got[name] = [3]int64{current, previous, delta}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected three comparison metrics: %v", got)
	}
	for name, want := range map[string][3]int64{"Поступило": {2, 1, 1}, "Закрыто": {1, 0, 1}, "Отменено": {1, 0, 1}} {
		if got[name] != want {
			t.Fatalf("%s: got %v want %v", name, got[name], want)
		}
	}
}
