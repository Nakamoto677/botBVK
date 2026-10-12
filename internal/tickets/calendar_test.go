package tickets

import (
	"testing"
	"time"
)

func TestBusinessCalendarWeekendAndHoliday(t *testing.T) {
	c := DefaultCalendar()
	c.Holidays = map[string]bool{"2026-10-12": true}
	from := time.Date(2026, 10, 9, 17, 30, 0, 0, c.Location)
	want := time.Date(2026, 10, 13, 9, 30, 0, 0, c.Location)
	if got := c.AddWorkingTime(from, time.Hour); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := c.WorkDuration(from, want); got != time.Hour {
		t.Fatalf("duration=%v", got)
	}
	if c.IsWorking(time.Date(2026, 10, 12, 10, 0, 0, 0, c.Location)) {
		t.Fatal("holiday is working")
	}
}
func TestBusinessCalendarBoundaries(t *testing.T) {
	c := DefaultCalendar()
	from := time.Date(2026, 10, 13, 18, 0, 0, 0, c.Location)
	want := time.Date(2026, 10, 14, 9, 30, 0, 0, c.Location)
	if got := c.AddWorkingTime(from, 30*time.Minute); !got.Equal(want) {
		t.Fatal(got)
	}
	if c.WorkDuration(want, from) != 0 {
		t.Fatal("reverse range")
	}
	if c.IsWorking(from) {
		t.Fatal("closing hour")
	}
}
