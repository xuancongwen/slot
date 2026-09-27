package slot

import (
	"context"
	"database/sql"
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
	rows, e := a.db.QueryContext(ctx, "SELECT id,kind,label,detail FROM locations WHERE user_id=? ORDER BY position,id", u.ID)
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
