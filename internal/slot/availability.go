package slot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Span struct{ Start, End time.Time }

type Slot struct {
	Start int64
	Label string
}

func overlaps(a, b Span) bool { return a.Start.Before(b.End) && a.End.After(b.Start) }

// Slots are evaluated in the meeting type's IANA timezone, then represented as UTC instants.
// Iterating instants rather than constructing wall times handles skipped/repeated DST hours.
func generateSlots(mt MeetingType, day time.Time, now time.Time, busy []Span) []Slot {
	loc, e := time.LoadLocation(mt.Timezone)
	if e != nil {
		return nil
	}
	day = day.In(loc)
	if !strings.Contains(mt.Days, fmt.Sprint(int(day.Weekday()))) {
		return nil
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	finish := start.AddDate(0, 0, 1)
	limit := now.In(loc).AddDate(0, 0, mt.Horizon)
	min := now.Add(time.Duration(mt.Notice) * time.Minute)
	var out []Slot
	for t := start; t.Before(finish); t = t.Add(time.Minute) {
		local := t.In(loc)
		minute := local.Hour()*60 + local.Minute()
		if minute < mt.StartMin || (minute-mt.StartMin)%mt.Duration != 0 || t.Before(min) || t.After(limit) {
			continue
		}
		end := t.Add(time.Duration(mt.Duration) * time.Minute)
		endLocal := end.In(loc)
		if end.After(finish) || endLocal.Year() != local.Year() || endLocal.YearDay() != local.YearDay() || endLocal.Hour()*60+endLocal.Minute() > mt.EndMin || minute >= mt.EndMin {
			continue
		}
		block := Span{t.Add(-time.Duration(mt.Buffer) * time.Minute), end.Add(time.Duration(mt.Buffer) * time.Minute)}
		available := true
		for _, b := range busy {
			if overlaps(block, b) {
				available = false
				break
			}
		}
		if available {
			out = append(out, Slot{Start: t.Unix(), Label: local.Format("15:04 MST (UTC-07:00)")})
		}
	}
	return out
}

// availability returns open slots starting in [from, to). Guests view the range in their own
// timezone, so it can straddle several of the meeting type's days; Google is asked once for all of it.
func (a *App) availability(ctx context.Context, u User, t MeetingType, from, to time.Time, now time.Time) ([]Slot, error) {
	if !u.Enabled || !t.Active || !u.WriteCalendar.Valid || !from.Before(to) {
		return nil, nil
	}
	loc, e := time.LoadLocation(t.Timezone)
	if e != nil {
		return nil, e
	}
	first := from.In(loc)
	first = time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, loc)
	var days []time.Time
	for d := first; d.Before(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}
	blocked := map[string]bool{}
	rows, e := a.db.QueryContext(ctx, "SELECT day FROM blocks WHERE user_id=? AND day BETWEEN ? AND ?", u.ID, days[0].Format("2006-01-02"), days[len(days)-1].Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var day string
		if e = rows.Scan(&day); e != nil {
			rows.Close()
			return nil, e
		}
		blocked[day] = true
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return nil, e
	}
	padding := time.Duration(t.Buffer) * time.Minute
	busyFrom := first.Add(-padding)
	busyTo := days[len(days)-1].AddDate(0, 0, 1).Add(padding)
	cs, e := a.calendars(ctx, u.ID)
	if e != nil {
		return nil, e
	}
	var selected []Calendar
	hasWrite := false
	for _, c := range cs {
		if c.CheckBusy || c.ID == u.WriteCalendar.Int64 {
			selected = append(selected, c)
		}
		if c.ID == u.WriteCalendar.Int64 && (c.Role == "owner" || c.Role == "writer") {
			hasWrite = true
		}
	}
	if !hasWrite {
		return nil, errors.New("booking calendar unavailable")
	}
	busy, e := a.google.Busy(ctx, selected, busyFrom, busyTo)
	if e != nil {
		return nil, e
	}
	rows, e = a.db.QueryContext(ctx, `SELECT block_start,block_end FROM bookings WHERE user_id=? AND status IN ('pending','confirmed','cancel_pending') AND block_start<? AND block_end>?`, u.ID, busyTo.Unix(), busyFrom.Unix())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var start, end int64
		if e = rows.Scan(&start, &end); e != nil {
			return nil, e
		}
		busy = append(busy, Span{time.Unix(start, 0), time.Unix(end, 0)})
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	var out []Slot
	for _, d := range days {
		if blocked[d.Format("2006-01-02")] {
			continue
		}
		for _, s := range generateSlots(t, d, now, busy) {
			if s.Start >= from.Unix() && s.Start < to.Unix() {
				out = append(out, s)
			}
		}
	}
	return out, nil
}
