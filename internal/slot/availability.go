package slot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Span struct{ Start, End time.Time }

// A Slot is a bookable start time. Label is filled in for display, in the viewer's timezone.
type Slot struct {
	Start int64
	Label string
}

func overlaps(a, b Span) bool { return a.Start.Before(b.End) && a.End.After(b.Start) }

// subtract returns what is left of s outside hole.
func subtract(s, hole Span) []Span {
	if !overlaps(s, hole) {
		return []Span{s}
	}
	var out []Span
	if s.Start.Before(hole.Start) {
		out = append(out, Span{s.Start, hole.Start})
	}
	if s.End.After(hole.End) {
		out = append(out, Span{hole.End, s.End})
	}
	return out
}

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
			out = append(out, Slot{Start: t.Unix()})
		}
	}
	return out
}

// availability returns open slots starting in [from, to). Guests view the range in their own
// timezone, so it can straddle several of the meeting type's days; Google is asked once for all of it.
// A booking being rescheduled is passed as moving, so its current time does not block its new one.
func (a *App) availability(ctx context.Context, u User, t MeetingType, from, to time.Time, now time.Time, moving Booking) ([]Slot, error) {
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
	daysOff, e := queryAll(ctx, a.db, scanDayOff, "SELECT id,first_day,last_day FROM blocks WHERE user_id=? AND first_day<=? AND last_day>=?", u.ID, days[len(days)-1].Format(dateLayout), days[0].Format(dateLayout))
	if e != nil {
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
		if c.ID == u.WriteCalendar.Int64 && c.Writable() {
			hasWrite = true
		}
	}
	if !hasWrite {
		return nil, errors.New("booking calendar unavailable")
	}
	busy, e := a.busy(ctx, selected, busyFrom, busyTo, moving)
	if e != nil {
		return nil, e
	}
	reserved, e := queryAll(ctx, a.db, func(s scanner) (Span, error) {
		var start, end int64
		e := s.Scan(&start, &end)
		return Span{time.Unix(start, 0), time.Unix(end, 0)}, e
	}, `SELECT block_start,block_end FROM bookings WHERE user_id=? AND status IN ('requested','pending','confirmed','cancel_pending') AND block_start<? AND block_end>? AND id<>?`, u.ID, busyTo.Unix(), busyFrom.Unix(), moving.ID)
	if e != nil {
		return nil, e
	}
	busy = append(busy, reserved...)
	var out []Slot
	for _, d := range days {
		if slices.ContainsFunc(daysOff, func(off DayOff) bool { return off.Covers(d.Format(dateLayout)) }) {
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

// busy asks Google when the calendars cs are busy. A booking being moved frees the time
// of its own event, which only its destination calendar holds; a request has no event yet.
func (a *App) busy(ctx context.Context, cs []Calendar, from, to time.Time, moving Booking) ([]Span, error) {
	i := slices.IndexFunc(cs, func(c Calendar) bool { return c.ID == moving.CalendarID })
	if i < 0 || (moving.Status != "pending" && moving.Status != "confirmed") {
		return a.google.Busy(ctx, cs, from, to)
	}
	own, e := a.google.Busy(ctx, cs[i:i+1], from, to)
	if e != nil {
		return nil, e
	}
	busy, e := a.google.Busy(ctx, slices.Delete(slices.Clone(cs), i, i+1), from, to)
	if e != nil {
		return nil, e
	}
	event := Span{time.Unix(moving.Start, 0), time.Unix(moving.End, 0)}
	for _, s := range own {
		busy = append(busy, subtract(s, event)...)
	}
	return busy, nil
}
