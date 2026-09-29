package slot

import (
	"context"
	"database/sql"
	"slices"
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
	// AllLocations offers every host location plus the guest's own. Otherwise only
	// the rows in meeting_type_locations, and the guest's own only with GuestLocation.
	AllLocations, GuestLocation bool
}

// AllowsGuestLocation reports whether guests may type a location of their own.
func (t MeetingType) AllowsGuestLocation() bool { return t.AllLocations || t.GuestLocation }

const meetingTypeColumns = "id,user_id,slug,name,timezone,days,start_min,end_min,duration,buffer,notice,horizon,active,approval,all_locations,guest_location"

func scanMeetingType(s scanner) (MeetingType, error) {
	var t MeetingType
	err := s.Scan(&t.ID, &t.UserID, &t.Slug, &t.Name, &t.Timezone, &t.Days, &t.StartMin, &t.EndMin, &t.Duration, &t.Buffer, &t.Notice, &t.Horizon, &t.Active, &t.Approval, &t.AllLocations, &t.GuestLocation)
	return t, err
}

func (a *App) meetingTypes(ctx context.Context, uid int64, activeOnly bool) ([]MeetingType, error) {
	return queryAll(ctx, a.db, scanMeetingType, "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE user_id=? AND (active=1 OR ?=0) ORDER BY duration,name", uid, activeOnly)
}

type Location struct {
	ID                  int64
	Kind, Label, Detail string
	Default             bool
	Offered             bool // By the meeting type being edited or booked.
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

// typeLocations returns all of u's locations, marking those t offers. When t offers
// only some and not u's default, the first offered one is preselected instead.
func (a *App) typeLocations(ctx context.Context, u User, t MeetingType) ([]Location, error) {
	ls, e := a.locations(ctx, u)
	if e != nil {
		return nil, e
	}
	offered, e := queryAll(ctx, a.db, func(s scanner) (int64, error) {
		var id int64
		return id, s.Scan(&id)
	}, "SELECT location_id FROM meeting_type_locations WHERE meeting_type_id=?", t.ID)
	if e != nil {
		return nil, e
	}
	hasDefault := false
	for i := range ls {
		ls[i].Offered = t.AllLocations || slices.Contains(offered, ls[i].ID)
		ls[i].Default = ls[i].Default && ls[i].Offered
		hasDefault = hasDefault || ls[i].Default
	}
	if i := slices.IndexFunc(ls, func(l Location) bool { return l.Offered }); i >= 0 && !hasDefault && !t.AllLocations {
		ls[i].Default = true
	}
	return ls, nil
}

// offeredLocations keeps the locations the meeting type offers.
func offeredLocations(ls []Location) []Location {
	return slices.DeleteFunc(ls, func(l Location) bool { return !l.Offered })
}

// A DayOff blocks every date from First through Last, as YYYY-MM-DD in each meeting type's timezone.
type DayOff struct {
	ID          int64
	First, Last string
}

// Covers reports whether day, as YYYY-MM-DD, falls in the range.
func (d DayOff) Covers(day string) bool { return d.First <= day && day <= d.Last }

func scanDayOff(s scanner) (DayOff, error) {
	var d DayOff
	return d, s.Scan(&d.ID, &d.First, &d.Last)
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
	Meet                                           bool
	Created                                        int64
	Attempts                                       int
	NextAttempt                                    int64
	LastError                                      string
}

const bookingColumns = "id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,reason,manage_token,status,created,attempts,next_attempt,last_error"

func scanBooking(s scanner) (Booking, error) {
	var b Booking
	e := s.Scan(&b.ID, &b.UserID, &b.CalendarID, &b.GuestName, &b.GuestEmail, &b.Start, &b.End, &b.BlockStart, &b.BlockEnd, &b.Title, &b.Location, &b.Meet, &b.Timezone, &b.GuestTimezone, &b.Reason, &b.ManageToken, &b.Status, &b.Created, &b.Attempts, &b.NextAttempt, &b.LastError)
	return b, e
}

func (a *App) getBooking(ctx context.Context, token string) (Booking, error) {
	return scanBooking(a.db.QueryRowContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE manage_token=?", token))
}

func (a *App) hostBookings(ctx context.Context, uid int64) ([]Booking, error) {
	return queryAll(ctx, a.db, scanBooking, "SELECT "+bookingColumns+" FROM bookings WHERE user_id=? ORDER BY start DESC LIMIT 100", uid)
}

func (a *App) reserve(ctx context.Context, b Booking) error {
	_, e := a.db.ExecContext(ctx, `INSERT INTO bookings (id,user_id,calendar_id,guest_name,guest_email,start,end,block_start,block_end,title,location,meet,timezone,guest_timezone,reason,manage_token,status,created) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, b.ID, b.UserID, b.CalendarID, b.GuestName, b.GuestEmail, b.Start, b.End, b.BlockStart, b.BlockEnd, b.Title, b.Location, b.Meet, b.Timezone, b.GuestTimezone, b.Reason, b.ManageToken, b.Status, b.Created)
	return e
}
