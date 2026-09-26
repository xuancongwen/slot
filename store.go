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
	ID                                                  int64
	Email, Password, Name, Slug, Timezone, Days         string
	StartMin, EndMin, Duration, Buffer, Notice, Horizon int
	Location                                            string
	Enabled                                             bool
	WriteCalendar                                       sql.NullInt64
}

const userColumns = "id,email,password,name,slug,timezone,days,start_min,end_min,duration,buffer,notice,horizon,location,enabled,write_calendar"

type scanner interface{ Scan(...any) error }

func scanUser(s scanner) (User, error) {
	var u User
	err := s.Scan(&u.ID, &u.Email, &u.Password, &u.Name, &u.Slug, &u.Timezone, &u.Days, &u.StartMin, &u.EndMin, &u.Duration, &u.Buffer, &u.Notice, &u.Horizon, &u.Location, &u.Enabled, &u.WriteCalendar)
	return u, err
}
func (a *App) userByID(ctx context.Context, id int64) (User, error) {
	return scanUser(a.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id=?", id))
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
	Created                                        int64
	Attempts                                       int
	NextAttempt                                    int64
	LastError                                      string
}

const bookingColumns = "id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,timezone,manage_token,status,created,attempts,next_attempt,last_error"

func scanBooking(s scanner) (Booking, error) {
	var b Booking
	e := s.Scan(&b.ID, &b.UserID, &b.CalendarID, &b.GuestName, &b.GuestEmail, &b.Start, &b.End, &b.BlockStart, &b.BlockEnd, &b.Title, &b.Location, &b.Timezone, &b.ManageToken, &b.Status, &b.Created, &b.Attempts, &b.NextAttempt, &b.LastError)
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
	_, e := a.db.ExecContext(ctx, `INSERT INTO bookings (id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,timezone,manage_token,status,created) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`, b.ID, b.UserID, b.CalendarID, b.GuestName, b.GuestEmail, b.Start, b.End, b.BlockStart, b.BlockEnd, b.Title, b.Location, b.Timezone, b.ManageToken, b.Created)
	return e
}

type Span struct{ Start, End time.Time }
type Slot struct {
	Start int64
	Label string
}

func overlaps(a, b Span) bool { return a.Start.Before(b.End) && a.End.After(b.Start) }

// Slots are evaluated in the host's IANA timezone, then represented as UTC instants.
// Iterating instants rather than constructing wall times handles skipped/repeated DST hours.
func generateSlots(u User, day time.Time, now time.Time, busy []Span) []Slot {
	loc, e := time.LoadLocation(u.Timezone)
	if e != nil {
		return nil
	}
	day = day.In(loc)
	if !strings.Contains(u.Days, fmt.Sprint(int(day.Weekday()))) {
		return nil
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	finish := start.AddDate(0, 0, 1)
	limit := now.In(loc).AddDate(0, 0, u.Horizon)
	min := now.Add(time.Duration(u.Notice) * time.Minute)
	var out []Slot
	for t := start; t.Before(finish); t = t.Add(time.Minute) {
		local := t.In(loc)
		minute := local.Hour()*60 + local.Minute()
		if minute < u.StartMin || (minute-u.StartMin)%u.Duration != 0 || t.Before(min) || t.After(limit) {
			continue
		}
		end := t.Add(time.Duration(u.Duration) * time.Minute)
		endLocal := end.In(loc)
		if end.After(finish) || endLocal.Year() != local.Year() || endLocal.YearDay() != local.YearDay() || endLocal.Hour()*60+endLocal.Minute() > u.EndMin || minute >= u.EndMin {
			continue
		}
		block := Span{t.Add(-time.Duration(u.Buffer) * time.Minute), end.Add(time.Duration(u.Buffer) * time.Minute)}
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

func (a *App) availability(ctx context.Context, u User, day time.Time, now time.Time) ([]Slot, error) {
	if !u.Enabled || !u.WriteCalendar.Valid {
		return nil, nil
	}
	var blocked int
	if e := a.db.QueryRowContext(ctx, "SELECT count(*) FROM blocks WHERE user_id=? AND day=?", u.ID, day.Format("2006-01-02")).Scan(&blocked); e != nil {
		return nil, e
	}
	if blocked > 0 {
		return nil, nil
	}
	from := day.Add(-time.Duration(u.Buffer) * time.Minute)
	to := day.AddDate(0, 0, 1).Add(time.Duration(u.Buffer) * time.Minute)
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
	busy, e := a.google.Busy(ctx, selected, from, to)
	if e != nil {
		return nil, e
	}
	rows, e := a.db.QueryContext(ctx, `SELECT block_start,block_end FROM bookings WHERE user_id=? AND status IN ('pending','confirmed','cancel_pending') AND block_start<? AND block_end>?`, u.ID, to.Unix(), from.Unix())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var s, t int64
		if e = rows.Scan(&s, &t); e != nil {
			return nil, e
		}
		busy = append(busy, Span{time.Unix(s, 0), time.Unix(t, 0)})
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return generateSlots(u, day, now, busy), nil
}
