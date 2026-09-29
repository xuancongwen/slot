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

// queryAll runs query and scans every row with scan.
func queryAll[T any](ctx context.Context, db *sql.DB, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, e := db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanString(s scanner) (string, error) {
	var v string
	return v, s.Scan(&v)
}

// inTx runs fn in a transaction, committing only if fn succeeds.
func (a *App) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := a.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}

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
	Active, Approval                                    bool
}

const meetingTypeColumns = "id,user_id,slug,name,timezone,days,start_min,end_min,duration,buffer,notice,horizon,active,approval"

func scanMeetingType(s scanner) (MeetingType, error) {
	var t MeetingType
	err := s.Scan(&t.ID, &t.UserID, &t.Slug, &t.Name, &t.Timezone, &t.Days, &t.StartMin, &t.EndMin, &t.Duration, &t.Buffer, &t.Notice, &t.Horizon, &t.Active, &t.Approval)
	return t, err
}

func (a *App) meetingTypes(ctx context.Context, uid int64, activeOnly bool) ([]MeetingType, error) {
	return queryAll(ctx, a.db, scanMeetingType, "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE user_id=? AND (active=1 OR ?=0) ORDER BY duration,name", uid, activeOnly)
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
	return queryAll(ctx, a.db, func(s scanner) (Location, error) {
		var l Location
		e := s.Scan(&l.ID, &l.Kind, &l.Label, &l.Detail)
		l.Default = u.DefaultLocation.Valid && l.ID == u.DefaultLocation.Int64
		return l, e
	}, "SELECT id,kind,label,detail FROM locations WHERE user_id=? ORDER BY position,id", u.ID)
}

type Calendar struct {
	ID, AccountID                  int64
	GoogleID, Name, Role, Identity string
	CheckBusy                      bool
}

// Writable reports whether Slot may insert bookings into the calendar.
func (c Calendar) Writable() bool { return c.Role == "owner" || c.Role == "writer" }

const calendarQuery = "SELECT c.id,c.account_id,c.google_id,c.name,c.role,a.identity,c.check_busy FROM calendars c JOIN accounts a ON a.id=c.account_id"

func scanCalendar(s scanner) (Calendar, error) {
	var c Calendar
	err := s.Scan(&c.ID, &c.AccountID, &c.GoogleID, &c.Name, &c.Role, &c.Identity, &c.CheckBusy)
	return c, err
}
func (a *App) calendars(ctx context.Context, uid int64) ([]Calendar, error) {
	return queryAll(ctx, a.db, scanCalendar, calendarQuery+" WHERE a.user_id=? ORDER BY a.identity,c.name", uid)
}

func (a *App) bookingCalendar(ctx context.Context, id int64) (Calendar, error) {
	return scanCalendar(a.db.QueryRowContext(ctx, calendarQuery+" WHERE c.id=?", id))
}

type Booking struct {
	ID                                             string
	UserID, CalendarID                             int64
	GuestName, GuestEmail                          string
	Start, End, BlockStart, BlockEnd               int64
	Title, Location, Timezone, ManageToken, Status string
	GuestTimezone, Reason                          string
	Meet, Verified                                 bool
	Created                                        int64
	Attempts                                       int
	NextAttempt                                    int64
	LastError                                      string
}

const bookingColumns = "id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,reason,manage_token,status,created,attempts,next_attempt,last_error,verified"

func scanBooking(s scanner) (Booking, error) {
	var b Booking
	e := s.Scan(&b.ID, &b.UserID, &b.CalendarID, &b.GuestName, &b.GuestEmail, &b.Start, &b.End, &b.BlockStart, &b.BlockEnd, &b.Title, &b.Location, &b.Meet, &b.Timezone, &b.GuestTimezone, &b.Reason, &b.ManageToken, &b.Status, &b.Created, &b.Attempts, &b.NextAttempt, &b.LastError, &b.Verified)
	return b, e
}

func (a *App) getBooking(ctx context.Context, token string) (Booking, error) {
	return scanBooking(a.db.QueryRowContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE manage_token=?", token))
}

func (a *App) hostBookings(ctx context.Context, uid int64) ([]Booking, error) {
	return queryAll(ctx, a.db, scanBooking, "SELECT "+bookingColumns+" FROM bookings WHERE user_id=? AND verified=1 ORDER BY start DESC LIMIT 100", uid)
}

func (a *App) reserve(ctx context.Context, b Booking) error {
	_, e := a.db.ExecContext(ctx, `INSERT INTO bookings (id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,reason,manage_token,status,created,verified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, b.ID, b.UserID, b.CalendarID, b.GuestName, b.GuestEmail, b.Start, b.End, b.BlockStart, b.BlockEnd, b.Title, b.Location, b.Meet, b.Timezone, b.GuestTimezone, b.Reason, b.ManageToken, b.Status, b.Created, b.Verified)
	return e
}
