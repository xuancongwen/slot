package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type User struct {
	ID                          int64
	Email, Password, Name, Slug string
	Enabled                     bool
	WriteCalendar               sql.NullInt64
	DefaultLocation             sql.NullInt64
}

const userColumns = "id,email,password,name,slug,enabled,write_calendar,default_location"

type scanner interface{ Scan(...any) error }

func scanUser(s scanner) (User, error) {
	var u User
	err := s.Scan(&u.ID, &u.Email, &u.Password, &u.Name, &u.Slug, &u.Enabled, &u.WriteCalendar, &u.DefaultLocation)
	return u, err
}
func (a *App) userByID(ctx context.Context, id int64) (User, error) {
	return scanUser(a.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id=?", id))
}

// A MeetingType carries its own timezone and hours, so a host can publish, say,
// a Singapore schedule for a trip alongside their usual Seattle one.
type MeetingType struct {
	ID, UserID                                          int64
	Slug, Name, Timezone, Days                          string
	StartMin, EndMin, Duration, Buffer, Notice, Horizon int
	Active                                              bool
}

const meetingTypeColumns = "id,user_id,slug,name,timezone,days,start_min,end_min,duration,buffer,notice,horizon,active"

func scanMeetingType(s scanner) (MeetingType, error) {
	var t MeetingType
	err := s.Scan(&t.ID, &t.UserID, &t.Slug, &t.Name, &t.Timezone, &t.Days, &t.StartMin, &t.EndMin, &t.Duration, &t.Buffer, &t.Notice, &t.Horizon, &t.Active)
	return t, err
}
func (a *App) meetingTypes(ctx context.Context, uid int64, activeOnly bool) ([]MeetingType, error) {
	rows, e := a.db.QueryContext(ctx, "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE user_id=? AND (active=1 OR ?=0) ORDER BY duration,name", uid, activeOnly)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ts []MeetingType
	for rows.Next() {
		t, e := scanMeetingType(rows)
		if e != nil {
			return nil, e
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

type Location struct {
	ID                  int64
	Kind, Label, Detail string
	Default             bool
}

// Text is what a booking records as its location: the link or address, or the label alone.
func (l Location) Text() string {
	if l.Detail != "" {
		return l.Detail
	}
	return l.Label
}

func (a *App) locations(ctx context.Context, u User) ([]Location, error) {
	rows, e := a.db.QueryContext(ctx, "SELECT id,kind,label,detail FROM locations WHERE user_id=? ORDER BY id", u.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ls []Location
	for rows.Next() {
		var l Location
		if e = rows.Scan(&l.ID, &l.Kind, &l.Label, &l.Detail); e != nil {
			return nil, e
		}
		l.Default = u.DefaultLocation.Valid && l.ID == u.DefaultLocation.Int64
		ls = append(ls, l)
	}
	return ls, rows.Err()
}

type Calendar struct {
	ID, AccountID                  int64
	GoogleID, Name, Role, Identity string
	CheckBusy                      bool
}

func (a *App) calendars(ctx context.Context, uid int64) ([]Calendar, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT c.id,c.account_id,c.google_id,c.name,c.role,a.identity,c.check_busy FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE a.user_id=? ORDER BY a.identity,c.name`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Calendar
	for rows.Next() {
		var c Calendar
		if err = rows.Scan(&c.ID, &c.AccountID, &c.GoogleID, &c.Name, &c.Role, &c.Identity, &c.CheckBusy); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (a *App) bookingCalendar(ctx context.Context, id int64) (Calendar, error) {
	var c Calendar
	err := a.db.QueryRowContext(ctx, `SELECT c.id,c.account_id,c.google_id,c.name,c.role,a.identity,c.check_busy FROM calendars c JOIN accounts a ON a.id=c.account_id WHERE c.id=?`, id).Scan(&c.ID, &c.AccountID, &c.GoogleID, &c.Name, &c.Role, &c.Identity, &c.CheckBusy)
	return c, err
}

type Booking struct {
	ID                                             string
	UserID, CalendarID                             int64
	GuestName, GuestEmail                          string
	Start, End, BlockStart, BlockEnd               int64
	Title, Location, Timezone, ManageToken, Status string
	GuestTimezone                                  string
	Meet                                           bool
	Created                                        int64
	Attempts                                       int
	NextAttempt                                    int64
	LastError                                      string
}

const bookingColumns = "id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,manage_token,status,created,attempts,next_attempt,last_error"

func scanBooking(s scanner) (Booking, error) {
	var b Booking
	e := s.Scan(&b.ID, &b.UserID, &b.CalendarID, &b.GuestName, &b.GuestEmail, &b.Start, &b.End, &b.BlockStart, &b.BlockEnd, &b.Title, &b.Location, &b.Meet, &b.Timezone, &b.GuestTimezone, &b.ManageToken, &b.Status, &b.Created, &b.Attempts, &b.NextAttempt, &b.LastError)
	return b, e
}
func (a *App) getBooking(ctx context.Context, token string) (Booking, error) {
	return scanBooking(a.db.QueryRowContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE manage_token=?", token))
}
func (a *App) hostBookings(ctx context.Context, uid int64) ([]Booking, error) {
	rows, e := a.db.QueryContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE user_id=? ORDER BY start DESC LIMIT 100", uid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var bs []Booking
	for rows.Next() {
		b, e := scanBooking(rows)
		if e != nil {
			return nil, e
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}
func (a *App) reserve(ctx context.Context, b Booking) error {
	_, e := a.db.ExecContext(ctx, `INSERT INTO bookings (id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,manage_token,status,created) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`, b.ID, b.UserID, b.CalendarID, b.GuestName, b.GuestEmail, b.Start, b.End, b.BlockStart, b.BlockEnd, b.Title, b.Location, b.Meet, b.Timezone, b.GuestTimezone, b.ManageToken, b.Created)
	return e
}

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
