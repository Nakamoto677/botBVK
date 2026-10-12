package tickets

import "time"

// Calendar counts weekday business hours in Location. Holidays are local YYYY-MM-DD dates.
// Zero-valued calendar uses Monday-Friday 09:00-18:00 Asia/Irkutsk.
type Calendar struct {
	Location  *time.Location
	StartHour int
	EndHour   int
	Holidays  map[string]bool
}

func DefaultCalendar() Calendar {
	loc, err := time.LoadLocation("Asia/Irkutsk")
	if err != nil {
		loc = time.FixedZone("Asia/Irkutsk", 8*3600)
	}
	return Calendar{Location: loc, StartHour: 9, EndHour: 18}
}
func (c Calendar) normalized() Calendar {
	if c.Location == nil {
		c.Location = DefaultCalendar().Location
	}
	if c.StartHour == 0 && c.EndHour == 0 {
		c.StartHour = 9
		c.EndHour = 18
	}
	return c
}
func (c Calendar) window(day time.Time) (time.Time, time.Time, bool) {
	day = day.In(c.Location)
	start := time.Date(day.Year(), day.Month(), day.Day(), c.StartHour, 0, 0, 0, c.Location)
	end := time.Date(day.Year(), day.Month(), day.Day(), c.EndHour, 0, 0, 0, c.Location)
	ok := day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !c.Holidays[day.Format("2006-01-02")]
	return start, end, ok
}
func (c Calendar) WorkDuration(from, to time.Time) time.Duration {
	c = c.normalized()
	if !to.After(from) || c.EndHour <= c.StartHour {
		return 0
	}
	cursor := from.In(c.Location)
	var total time.Duration
	for cursor.Before(to) {
		start, end, ok := c.window(cursor)
		if ok {
			if cursor.After(start) {
				start = cursor
			}
			if to.Before(end) {
				end = to
			}
			if end.After(start) {
				total += end.Sub(start)
			}
		}
		cursor = time.Date(cursor.Year(), cursor.Month(), cursor.Day()+1, 0, 0, 0, 0, c.Location)
	}
	return total
}
func (c Calendar) AddWorkingTime(from time.Time, amount time.Duration) time.Time {
	c = c.normalized()
	if amount <= 0 || c.EndHour <= c.StartHour {
		return from
	}
	cursor := from.In(c.Location)
	for {
		start, end, ok := c.window(cursor)
		if ok {
			if cursor.After(start) {
				start = cursor
			}
			if end.After(start) {
				available := end.Sub(start)
				if amount <= available {
					return start.Add(amount)
				}
				amount -= available
			}
		}
		cursor = time.Date(cursor.Year(), cursor.Month(), cursor.Day()+1, 0, 0, 0, 0, c.Location)
	}
}
func (c Calendar) IsWorking(at time.Time) bool {
	c = c.normalized()
	start, end, ok := c.window(at)
	return ok && !at.Before(start) && at.Before(end)
}
