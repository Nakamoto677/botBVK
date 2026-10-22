package analytics

import (
	"encoding/json"
	"sort"
	"time"

	"bot-bvk/internal/domain"
)

// Calculate reconstructs history from facts in the snapshot. Current ticket
// fields are used for current attention flags, never as historical assignees.
func Calculate(src Source, opt Options) Snapshot {
	out := Snapshot{At: src.At, Departments: opt.Departments, Tickets: []Ticket{}, Cycles: []Cycle{}, Events: []Event{}, Intervals: []Interval{}, Users: src.Users, Companies: src.Companies}
	users := make(map[int64]domain.User, len(src.Users))
	for _, u := range src.Users {
		users[u.ID] = u
	}
	events := map[int64][]domain.Event{}
	messages := map[int64]map[int64]domain.Message{}
	for _, e := range src.Events {
		if !e.CreatedAt.After(src.At) {
			events[e.TicketID] = append(events[e.TicketID], e)
		}
	}
	for _, m := range src.Messages {
		if !m.CreatedAt.After(src.At) {
			if messages[m.TicketID] == nil {
				messages[m.TicketID] = map[int64]domain.Message{}
			}
			messages[m.TicketID][m.ID] = m
		}
	}
	all := append([]domain.Ticket(nil), src.Tickets...)
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	for _, t := range all {
		ev := events[t.ID]
		sort.SliceStable(ev, func(i, j int) bool {
			if ev[i].CreatedAt.Equal(ev[j].CreatedAt) {
				return ev[i].ID < ev[j].ID
			}
			return ev[i].CreatedAt.Before(ev[j].CreatedAt)
		})
		calculateTicket(&out, t, ev, messages[t.ID], users, opt)
	}
	return out
}

type eventData struct {
	From               json.RawMessage   `json:"from"`
	To                 json.RawMessage   `json:"to"`
	Department         domain.Department `json:"department"`
	Status             domain.Status     `json:"status"`
	Reason             string            `json:"reason"`
	Resolution         string            `json:"resolution"`
	SourceID           int64             `json:"source_message_id"`
	EditedAt           *time.Time        `json:"edited_at"`
	Direction          string            `json:"direction"`
	InitialDescription bool              `json:"initial_description"`
}

func payload(e domain.Event) (eventData, bool) {
	var p eventData
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return p, true
	}
	err := json.Unmarshal(e.Data, &p)
	return p, err == nil
}
func department(raw json.RawMessage) *domain.Department {
	var d domain.Department
	if json.Unmarshal(raw, &d) != nil || !d.Valid() {
		return nil
	}
	return &d
}
func number(raw json.RawMessage) *int64 {
	var n int64
	if json.Unmarshal(raw, &n) != nil || n <= 0 {
		return nil
	}
	return &n
}
func id(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}
func instant(t time.Time) *time.Time   { return &t }
func seconds(d time.Duration) *float64 { n := d.Seconds(); return &n }
func business(kind string) bool {
	switch kind {
	case "created", "claimed", "reassigned", "transferred", "reopened", "status_changed", "closed", "cancelled":
		return true
	}
	return false
}
func sameDepartment(a, b *domain.Department) bool { return a != nil && b != nil && *a == *b }

// Reverse traversal uses the stored final department only when an unbroken
// chain of transfer facts supports it. An absent/invalid transfer breaks that
// inference until a preceding explicit department anchor is encountered.
func departments(t domain.Ticket, events []domain.Event) []*domain.Department {
	result := make([]*domain.Department, len(events))
	var current *domain.Department
	hasCreation := false
	for _, e := range events {
		if e.Kind == "created" {
			hasCreation = true
		}
	}
	if hasCreation && t.Department.Valid() {
		d := t.Department
		current = &d
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		p, valid := payload(e)
		switch e.Kind {
		case "transferred":
			from, to := department(p.From), department(p.To)
			if !valid {
				from, to = nil, nil
			}
			// The department of a transfer is its source; target is a separate field.
			result[i] = from
			if to != nil && current != nil && !sameDepartment(to, current) {
				result[i] = nil
			}
			current = from
		case "reopened":
			d := department(p.From)
			if d != nil {
				current = d
			}
			result[i] = current
		case "created":
			if p.Department.Valid() {
				d := p.Department
				current = &d
			}
			result[i] = current
		default:
			result[i] = current
		}
	}
	// Carry explicit historical anchors forward even when creation is missing.
	var forward *domain.Department
	for i, e := range events {
		p, _ := payload(e)
		if result[i] == nil {
			result[i] = forward
		} else {
			forward = result[i]
		}
		if e.Kind == "transferred" {
			forward = department(p.To)
		}
	}
	return result
}

func calculateTicket(out *Snapshot, t domain.Ticket, events []domain.Event, msgs map[int64]domain.Message, users map[int64]domain.User, opt Options) {
	row := Ticket{ID: t.ID, ClientID: t.ClientID, CompanyID: t.CompanyID, Description: t.Description, Status: t.Status, Department: t.Department, AssigneeID: id(t.AssigneeID), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, Quality: "complete"}
	row.ClientName = users[t.ClientID].Name
	deps := departments(t, events)
	created := len(events) > 0 && events[0].Kind == "created" && !t.CreatedAt.IsZero() && !events[0].CreatedAt.Before(t.CreatedAt)
	var dep *domain.Department
	if created {
		dep = deps[0]
	}
	state := domain.Status("")
	var owner *int64
	ownerKnown := false
	cycleBase := len(out.Cycles)
	intervalBase := len(out.Intervals)
	cycleIndex := -1
	cycleNumber := 0
	cycleStartKnown := map[int]bool{}
	incomplete := !created
	intervalStart := t.CreatedAt
	intervalGap := !created
	addInterval := func(end *time.Time) {
		if intervalStart.IsZero() {
			return
		}
		if end != nil && end.Before(intervalStart) {
			incomplete = true
			return
		}
		out.Intervals = append(out.Intervals, Interval{TicketID: t.ID, StartedAt: intervalStart, EndedAt: end, Status: state, Department: dep, AssigneeID: owner, CycleNumber: cycleNumber, Incomplete: intervalGap || dep == nil || !ownerKnown || state == ""})
	}
	startCycle := func(at time.Time, gap bool) {
		cycleNumber++
		out.Cycles = append(out.Cycles, Cycle{TicketID: t.ID, Number: cycleNumber, StartedAt: at, Department: dep, Outcome: "active", Incomplete: gap || dep == nil})
		cycleIndex = len(out.Cycles) - 1
	}
	if !created && !t.CreatedAt.IsZero() {
		startCycle(t.CreatedAt, true)
	}
	for i, e := range events {
		p, valid := payload(e)
		normalized := Event{ID: e.ID, TicketID: e.TicketID, Kind: e.Kind, At: e.CreatedAt, ActorID: id(e.ActorID), Department: deps[i], CycleNumber: cycleNumber, Incomplete: !valid || deps[i] == nil}
		if e.Kind == "transferred" {
			normalized.FromDepartment = department(p.From)
			normalized.ToDepartment = department(p.To)
			normalized.Detail = p.Reason
		}
		if e.Kind == "closed" {
			normalized.Detail = p.Resolution
		}
		if business(e.Kind) {
			if e.CreatedAt.Before(t.CreatedAt) {
				normalized.Incomplete = true
				incomplete = true
				out.Events = append(out.Events, normalized)
				continue
			}
			if e.Kind != "created" || !created {
				addInterval(instant(e.CreatedAt))
			}
			intervalStart = e.CreatedAt
			if deps[i] != nil {
				dep = deps[i]
			}
			switch e.Kind {
			case "created":
				if cycleIndex >= 0 {
					normalized.Incomplete = true
					incomplete = true
				} else {
					startCycle(e.CreatedAt, !created)
					cycleStartKnown[cycleIndex] = true
				}
				state = domain.New
				owner = nil
				ownerKnown = true
				intervalGap = !created
			case "reopened":
				if cycleIndex >= 0 && out.Cycles[cycleIndex].EndedAt == nil {
					out.Cycles[cycleIndex].Incomplete = true
					incomplete = true
					normalized.Incomplete = true
				}
				state = domain.New
				owner = nil
				ownerKnown = true
				intervalGap = normalized.Incomplete
				startCycle(e.CreatedAt, intervalGap)
				cycleStartKnown[cycleIndex] = true
			case "claimed":
				if state != domain.New {
					normalized.Incomplete = true
				}
				state = domain.InProgress
				owner = id(e.ActorID)
				ownerKnown = owner != nil
				normalized.AssigneeID = owner
				if row.FirstAssigneeID == nil && owner != nil && !incomplete {
					row.FirstAssigneeID = owner
				}
			case "reassigned":
				target := number(p.To)
				if target == nil || !state.Active() {
					normalized.Incomplete = true
				}
				if prior := number(p.From); prior == nil || (ownerKnown && owner != nil && *owner != *prior) {
					normalized.Incomplete = true
				}
				owner = target
				ownerKnown = target != nil
				normalized.AssigneeID = owner
				if row.FirstAssigneeID == nil && owner != nil && !incomplete {
					row.FirstAssigneeID = owner
				}
			case "transferred":
				if !state.Active() || normalized.FromDepartment == nil || normalized.ToDepartment == nil {
					normalized.Incomplete = true
				}
				dep = normalized.ToDepartment
				state = domain.New
				owner = nil
				ownerKnown = true
			case "status_changed":
				if p.Status != domain.InProgress && p.Status != domain.Waiting {
					normalized.Incomplete = true
					state = ""
				} else {
					if !((state == domain.InProgress && p.Status == domain.Waiting) || (state == domain.Waiting && p.Status == domain.InProgress)) {
						normalized.Incomplete = true
					}
					state = p.Status
				}
			case "closed", "cancelled":
				if !state.Active() || e.ActorID <= 0 {
					normalized.Incomplete = true
				}
				if e.Kind == "closed" {
					state = domain.Closed
				} else {
					state = domain.Cancelled
				}
				if cycleIndex < 0 || out.Cycles[cycleIndex].EndedAt != nil {
					normalized.Incomplete = true
				} else {
					c := &out.Cycles[cycleIndex]
					c.EndedAt = instant(e.CreatedAt)
					c.Outcome = e.Kind
					c.Department = dep
					c.ClosedBy = id(e.ActorID)
					c.Resolution = p.Resolution
				}
			}
			status := state
			normalized.Status = &status
			normalized.CycleNumber = cycleNumber
			if normalized.Incomplete || !ownerKnown || dep == nil {
				intervalGap = true
				incomplete = true
				if cycleIndex >= 0 {
					out.Cycles[cycleIndex].Incomplete = true
				}
			}
		}
		if e.Kind == "message_delivered" && valid && p.EditedAt == nil && cycleIndex >= 0 && cycleStartKnown[cycleIndex] {
			m, found := msgs[p.SourceID]
			c := &out.Cycles[cycleIndex]
			if found && m.Direction == "outbound" && (p.Direction == "" || p.Direction == "outbound") && m.AuthorID > 0 && !m.CreatedAt.Before(c.StartedAt) && !m.CreatedAt.After(e.CreatedAt) && (c.EndedAt == nil || e.CreatedAt.Before(*c.EndedAt)) {
				if c.FirstResponseAt == nil {
					c.FirstResponseAt = instant(e.CreatedAt)
					c.FirstResponseBy = id(m.AuthorID)
					c.FirstResponseDepartment = normalized.Department
				}
			} else if !found && !(p.InitialDescription && p.SourceID == 0 && p.Direction == "inbound") {
				normalized.Incomplete = true
				incomplete = true
			}
		}
		out.Events = append(out.Events, normalized)
	}
	addInterval(nil)
	// A current-state mismatch is evidence of missing business history.
	if state != t.Status || (dep != nil && *dep != t.Department) || (ownerKnown && ((owner == nil && t.AssigneeID != 0) || (owner != nil && *owner != t.AssigneeID))) {
		incomplete = true
		if len(out.Intervals) > intervalBase {
			out.Intervals[len(out.Intervals)-1].Incomplete = true
		}
		if cycleIndex >= 0 {
			out.Cycles[cycleIndex].Incomplete = true
		}
	}
	for ci := cycleBase; ci < len(out.Cycles); ci++ {
		c := &out.Cycles[ci]
		end := out.At
		if c.EndedAt != nil {
			end = *c.EndedAt
		}
		if c.FirstResponseAt != nil {
			c.FirstResponseSeconds = seconds(c.FirstResponseAt.Sub(c.StartedAt))
			c.FirstResponseWorkSeconds = seconds(opt.Calendar.WorkDuration(c.StartedAt, *c.FirstResponseAt))
		}
		if !c.Incomplete && !end.Before(c.StartedAt) {
			c.CalendarSeconds = seconds(end.Sub(c.StartedAt))
			c.WorkSeconds = seconds(opt.Calendar.WorkDuration(c.StartedAt, end))
			var waiting, work time.Duration
			for _, in := range out.Intervals[intervalBase:] {
				if in.CycleNumber != c.Number || in.Status != domain.Waiting {
					continue
				}
				stop := end
				if in.EndedAt != nil && in.EndedAt.Before(stop) {
					stop = *in.EndedAt
				}
				waiting += stop.Sub(in.StartedAt)
				work += opt.Calendar.WorkDuration(in.StartedAt, stop)
			}
			c.WaitingSeconds = seconds(waiting)
			c.WaitingWorkSeconds = seconds(work)
			c.WorkWithoutWaitingSeconds = seconds(opt.Calendar.WorkDuration(c.StartedAt, end) - work)
		}
	}
	if cycleIndex >= 0 {
		c := out.Cycles[cycleIndex]
		row.CycleStartedAt = instant(c.StartedAt)
		row.FirstResponseAt = c.FirstResponseAt
		row.FirstResponseBy = c.FirstResponseBy
		row.FirstResponseWorkSeconds = c.FirstResponseWorkSeconds
	}
	// Match scheduler attention baselines, including queued engineer messages.
	baseline := t.CreatedAt
	for _, m := range msgs {
		if (m.Direction == "outbound" || m.Direction == "internal") && m.CreatedAt.After(baseline) {
			baseline = m.CreatedAt
		}
	}
	waitBaseline := t.CreatedAt
	for _, e := range events {
		switch e.Kind {
		case "claimed", "reassigned", "reopened", "transferred", "status_changed":
			if e.CreatedAt.After(baseline) {
				baseline = e.CreatedAt
			}
		}
		if e.Kind == "status_changed" && e.CreatedAt.After(waitBaseline) {
			waitBaseline = e.CreatedAt
		}
	}
	if !t.CreatedAt.IsZero() {
		row.LastActivityAt = instant(baseline)
		if t.Status == domain.InProgress {
			row.IdleWorkSeconds = seconds(opt.Calendar.WorkDuration(baseline, out.At))
		}
		if t.Status == domain.Waiting {
			row.WaitingSince = instant(waitBaseline)
			row.WaitingWorkSeconds = seconds(opt.Calendar.WorkDuration(waitBaseline, out.At))
		}
		if row.CycleStartedAt != nil {
			if t.Status.Active() && row.FirstResponseAt == nil && !out.Cycles[cycleIndex].Incomplete {
				row.ResponseWaitingWorkSeconds = seconds(opt.Calendar.WorkDuration(*row.CycleStartedAt, out.At))
			}
			row.ResponseOverdue = t.Status.Active() && row.FirstResponseAt == nil && opt.Calendar.WorkDuration(*row.CycleStartedAt, out.At) >= time.Duration(opt.FirstResponseMinutes)*time.Minute
		}
		row.IdleOverdue = t.Status == domain.InProgress && opt.Calendar.WorkDuration(baseline, out.At) >= time.Duration(opt.IdleHours)*time.Hour
		hours := opt.Calendar.EndHour - opt.Calendar.StartHour
		if opt.Calendar.StartHour == 0 && opt.Calendar.EndHour == 0 {
			hours = 9
		}
		row.WaitingOverdue = t.Status == domain.Waiting && opt.Calendar.WorkDuration(waitBaseline, out.At) >= time.Duration(opt.WaitingDays*hours)*time.Hour
	}
	row.NoAssignee = t.Status.Active() && t.AssigneeID == 0
	row.Incomplete = incomplete
	if incomplete {
		row.Quality = "Есть пробелы или противоречия в истории; неизвестные сроки и участники не восстановлены"
		out.IncompleteCount++
	}
	out.Tickets = append(out.Tickets, row)
}
