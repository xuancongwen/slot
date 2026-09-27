package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Page struct {
	Title, CSRF, Error, Notice                          string
	Admin, Register, RegistrationOpen, GoogleConfigured bool
	User                                                User
	Calendars                                           []Calendar
	Accounts                                            []Account
	Bookings                                            []Booking
	Blocks                                              []string
	Days                                                []DayOption
	MeetingTypes                                        []MeetingType
	Locations                                           []Location
	MeetingType                                         MeetingType
	Timezones                                           []string
	Slots                                               []Slot
	Weeks                                               [][]CalendarDay
	Date, BookingURL, PublicURL                         string
	Month, MonthLabel, PrevMonth, NextMonth             string
	GuestTimezone, SelectedLabel                        string
	DetectTimezone                                      bool
	Booking                                             Booking
	Start                                               int64
	SlotLabel, Ticket                                   string
}
type Account struct {
	ID       int64
	Identity string
}
type DayOption struct {
	Number int
	Name   string
}

var days = []DayOption{{1, "Mon"}, {2, "Tue"}, {3, "Wed"}, {4, "Thu"}, {5, "Fri"}, {6, "Sat"}, {0, "Sun"}}
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, p Page, status int) {
	p.CSRF = a.csrfToken(w, r, p.Admin)
	p.PublicURL = a.cfg.PublicURL
	var buf bytes.Buffer
	if e := a.templates.ExecuteTemplate(&buf, name, p); e != nil {
		slog.Error("render", "template", name, "error", e)
		http.Error(w, "Could not render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}
func (a *App) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	a.render(w, r, "error", Page{Title: "Something needs attention", Error: message}, status)
}
func (a *App) internal(w http.ResponseWriter, r *http.Request, e error) {
	slog.Error("request", "path", r.URL.Path, "error", e)
	a.fail(w, r, 500, "Something went wrong. Please try again.")
}
func (a *App) commonRoutes(m *http.ServeMux) {
	sub, _ := fs.Sub(assets, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := a.db.PingContext(r.Context()); e != nil {
			http.Error(w, "unhealthy", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
}
func (a *App) publicHandler() http.Handler {
	m := http.NewServeMux()
	a.commonRoutes(m)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		a.render(w, r, "home", Page{Title: "Home"}, 200)
	})
	m.HandleFunc("GET /b/{slug}", a.hostPage)
	m.HandleFunc("GET /b/{slug}/{type}", a.publicPage)
	m.HandleFunc("POST /b/{slug}/{type}", a.book)
	m.HandleFunc("GET /manage/{token}", a.manage)
	m.HandleFunc("POST /manage/{token}/cancel", a.cancelPublic)
	m.HandleFunc("GET /manage/{token}/event.ics", a.ics)
	return a.middleware(m, false)
}
func (a *App) adminHandler() http.Handler {
	m := http.NewServeMux()
	a.commonRoutes(m)
	m.HandleFunc("GET /login", a.authPage)
	m.HandleFunc("POST /login", a.login)
	m.HandleFunc("GET /register", a.authPage)
	m.HandleFunc("POST /register", a.register)
	m.HandleFunc("GET /{$}", a.authenticated(a.dashboard))
	m.HandleFunc("POST /logout", a.authenticated(a.logout))
	m.HandleFunc("POST /settings", a.authenticated(a.settings))
	m.HandleFunc("GET /types/new", a.authenticated(a.meetingTypePage))
	m.HandleFunc("POST /types", a.authenticated(a.saveMeetingType))
	m.HandleFunc("GET /types/{id}", a.authenticated(a.meetingTypePage))
	m.HandleFunc("POST /types/{id}", a.authenticated(a.saveMeetingType))
	m.HandleFunc("POST /types/{id}/delete", a.authenticated(a.deleteMeetingType))
	m.HandleFunc("POST /locations", a.authenticated(a.addLocation))
	m.HandleFunc("POST /locations/{id}/default", a.authenticated(a.defaultLocation))
	m.HandleFunc("POST /locations/{id}/delete", a.authenticated(a.deleteLocation))
	m.HandleFunc("POST /calendars", a.authenticated(a.saveCalendarSettings))
	m.HandleFunc("POST /blocks", a.authenticated(a.blockDay))
	m.HandleFunc("POST /blocks/delete", a.authenticated(a.unblockDay))
	m.HandleFunc("POST /oauth/google", a.authenticated(a.oauthStart))
	m.HandleFunc("GET /oauth/callback", a.authenticated(a.oauthCallback))
	m.HandleFunc("POST /calendars/refresh", a.authenticated(a.refreshCalendars))
	m.HandleFunc("POST /bookings/{id}/cancel", a.authenticated(a.cancelAdmin))
	m.HandleFunc("POST /bookings/{id}/retry", a.authenticated(a.retryAdmin))
	m.HandleFunc("POST /password", a.authenticated(a.changePassword))
	return a.middleware(m, true)
}
func validEmail(s string) bool {
	m, e := mail.ParseAddress(s)
	return e == nil && m.Address == s && len(s) <= 254 && !strings.ContainsAny(s, "\r\n")
}
func (a *App) authPage(w http.ResponseWriter, r *http.Request) {
	reg := r.URL.Path == "/register"
	if reg && !a.cfg.RegistrationOpen {
		a.fail(w, r, 403, "Registration is closed on this server.")
		return
	}
	title := "Sign in"
	if reg {
		title = "Create account"
	}
	a.render(w, r, "auth", Page{Title: title, Admin: true, Register: reg, RegistrationOpen: a.cfg.RegistrationOpen}, 200)
}
func (a *App) authCapacity(w http.ResponseWriter) bool {
	select {
	case a.authSlots <- struct{}{}:
		return true
	default:
		http.Error(w, "Please try again in a moment", 429)
		return false
	}
}
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.RegistrationOpen {
		a.fail(w, r, 403, "Registration is closed.")
		return
	}
	if a.cfg.RegistrationCode != "" && subtle.ConstantTimeCompare([]byte(r.PostForm.Get("code")), []byte(a.cfg.RegistrationCode)) != 1 {
		a.fail(w, r, 403, "The registration code is incorrect.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	name := strings.TrimSpace(r.PostForm.Get("name"))
	slug := strings.ToLower(strings.TrimSpace(r.PostForm.Get("slug")))
	password := r.PostForm.Get("password")
	if !validEmail(email) || len(name) < 1 || len(name) > 100 || !slugPattern.MatchString(slug) || len(password) < 12 || len(password) > 256 {
		a.fail(w, r, 400, "Use a valid email, a name up to 100 characters, a URL name of 1–40 lowercase letters/digits/hyphens, and a password of 12–256 characters.")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	hash := passwordHash(password)
	<-a.authSlots
	// The browser fills in its own timezone; without JavaScript the starter type uses UTC.
	tz := r.PostForm.Get("timezone")
	if !validTimezone(tz) {
		tz = "UTC"
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	var id int64
	e = tx.QueryRowContext(r.Context(), "INSERT INTO users(email,password,name,slug,created) VALUES(?,?,?,?,?) RETURNING id", email, hash, name, slug, time.Now().Unix()).Scan(&id)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			a.fail(w, r, 409, "That email or booking URL is already registered.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	_, e = tx.ExecContext(r.Context(), "INSERT INTO meeting_types(user_id,slug,name,timezone) VALUES(?,'30min','30 minute meeting',?)", id, tz)
	var meet int64
	if e == nil {
		e = tx.QueryRowContext(r.Context(), "INSERT INTO locations(user_id,kind,label) VALUES(?,'meet','Google Meet') RETURNING id", id).Scan(&meet)
	}
	if e == nil {
		_, e = tx.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=?", meet, id)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e == nil {
		e = a.loginSession(w, r, id)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	password := r.PostForm.Get("password")
	if len(password) > 256 {
		a.fail(w, r, 400, "Invalid credentials")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	defer func() { <-a.authSlots }()
	u, e := scanUser(a.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users WHERE email=?", strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))))
	hash := u.Password
	if e != nil {
		hash = "00000000000000000000000000000000:0000000000000000000000000000000000000000000000000000000000000000"
	}
	match := passwordMatches(hash, password)
	if e != nil || !match {
		a.render(w, r, "auth", Page{Title: "Sign in", Admin: true, RegistrationOpen: a.cfg.RegistrationOpen, Error: "Email or password is incorrect."}, 401)
		return
	}
	if e = a.loginSession(w, r, u.ID); e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/", 303)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("slot_session")
	if _, e := a.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token=?", hashToken(c.Value)); e != nil {
		a.internal(w, r, e)
		return
	}
	a.cookie(w, "slot_session", "", -1, true)
	http.Redirect(w, r, "/login", 303)
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	p := Page{Title: "Dashboard", Admin: true, User: u, Days: days, GoogleConfigured: a.cfg.GoogleClientID != "", BookingURL: a.cfg.PublicURL + "/b/" + u.Slug}
	var e error
	p.Calendars, e = a.calendars(r.Context(), u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	p.MeetingTypes, e = a.meetingTypes(r.Context(), u.ID, false)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	p.Locations, e = a.locations(r.Context(), u)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	p.Bookings, e = a.hostBookings(r.Context(), u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	rows, e := a.db.QueryContext(r.Context(), "SELECT id,identity FROM accounts WHERE user_id=? ORDER BY id", u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	for rows.Next() {
		var ac Account
		if e = rows.Scan(&ac.ID, &ac.Identity); e != nil {
			break
		}
		p.Accounts = append(p.Accounts, ac)
	}
	rowErr := rows.Err()
	rows.Close()
	if e != nil || rowErr != nil {
		a.internal(w, r, errors.Join(e, rowErr))
		return
	}
	rows, e = a.db.QueryContext(r.Context(), "SELECT day FROM blocks WHERE user_id=? ORDER BY day", u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	for rows.Next() {
		var day string
		if e = rows.Scan(&day); e != nil {
			break
		}
		p.Blocks = append(p.Blocks, day)
	}
	rowErr = rows.Err()
	rows.Close()
	if e != nil || rowErr != nil {
		a.internal(w, r, errors.Join(e, rowErr))
		return
	}
	notices := map[string]string{"saved": "Your settings are saved.", "connected": "Google account connected. Choose which calendars to check and where bookings should go.", "refreshed": "Calendar list refreshed.", "cancelled": "Cancellation requested. The slot stays reserved until Google confirms.", "retry": "Calendar sync retried.", "password": "Password changed. Other sessions have been signed out.", "deleted": "Meeting type deleted."}
	p.Notice = notices[r.URL.Query().Get("notice")]
	a.render(w, r, "dashboard", p, 200)
}
func parseMinutes(s string) (int, error) {
	t, e := time.Parse("15:04", s)
	if e != nil {
		return 0, e
	}
	return t.Hour()*60 + t.Minute(), nil
}

// "Local" would silently follow the server's zone rather than the host's.
func validTimezone(tz string) bool {
	_, e := time.LoadLocation(tz)
	return tz != "" && tz != "Local" && e == nil
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) < 1 || len(name) > 100 {
		a.fail(w, r, 400, "Use a display name up to 100 characters.")
		return
	}
	enabled := r.PostForm.Get("enabled") == "on"
	if enabled && !u.WriteCalendar.Valid {
		a.fail(w, r, 400, "Select a destination calendar before publishing.")
		return
	}
	_, e := a.db.ExecContext(r.Context(), `UPDATE users SET name=?,enabled=? WHERE id=?`, name, enabled, u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
}
func (a *App) addLocation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	kind, label, detail := "custom", strings.TrimSpace(r.PostForm.Get("label")), strings.TrimSpace(r.PostForm.Get("detail"))
	if r.PostForm.Get("kind") == "meet" {
		kind, label, detail = "meet", "Google Meet", ""
	} else if len(label) < 1 || len(label) > 60 || len(detail) > 500 {
		a.fail(w, r, 400, "Give the location a name up to 60 characters, and a link or address up to 500.")
		return
	}
	var id int64
	e := a.db.QueryRowContext(r.Context(), "INSERT INTO locations(user_id,kind,label,detail) VALUES(?,?,?,?) RETURNING id", u.ID, kind, label, detail).Scan(&id)
	if e == nil && !u.DefaultLocation.Valid {
		_, e = a.db.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=?", id, u.ID)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved#profile", 303)
}
func (a *App) defaultLocation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	_, e := a.db.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=? AND EXISTS(SELECT 1 FROM locations WHERE id=? AND user_id=?)", r.PathValue("id"), u.ID, r.PathValue("id"), u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved#profile", 303)
}
func (a *App) deleteLocation(w http.ResponseWriter, r *http.Request) {
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM locations WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved#profile", 303)
}

// ownedMeetingType returns the zero MeetingType with no error for /types/new.
func (a *App) ownedMeetingType(r *http.Request) (MeetingType, error) {
	if r.PathValue("id") == "" {
		return MeetingType{Days: "12345", StartMin: 540, EndMin: 1020, Duration: 30, Notice: 120, Horizon: 30, Active: true}, nil
	}
	return scanMeetingType(a.db.QueryRowContext(r.Context(), "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID))
}
func (a *App) meetingTypePage(w http.ResponseWriter, r *http.Request) {
	t, e := a.ownedMeetingType(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	title := "New meeting type"
	if t.ID != 0 {
		title = t.Name
	}
	a.render(w, r, "meeting_type", Page{Title: title, Admin: true, User: currentUser(r), MeetingType: t, Days: days, Timezones: a.timezones, BookingURL: a.cfg.PublicURL + "/b/" + currentUser(r).Slug}, 200)
}
func (a *App) saveMeetingType(w http.ResponseWriter, r *http.Request) {
	t, e := a.ownedMeetingType(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	var e1, e2, e3, e4, e5, e6 error
	t.Name = strings.TrimSpace(f.Get("name"))
	t.Slug = strings.ToLower(strings.TrimSpace(f.Get("slug")))
	t.Timezone = strings.TrimSpace(f.Get("timezone"))
	t.StartMin, e1 = parseMinutes(f.Get("start"))
	t.EndMin, e2 = parseMinutes(f.Get("end"))
	t.Duration, e3 = strconv.Atoi(f.Get("duration"))
	t.Buffer, e4 = strconv.Atoi(f.Get("buffer"))
	t.Notice, e5 = strconv.Atoi(f.Get("notice"))
	t.Horizon, e6 = strconv.Atoi(f.Get("horizon"))
	t.Active = f.Get("active") == "on"
	t.Days = ""
	for _, d := range days {
		for _, v := range f["days"] {
			if v == strconv.Itoa(d.Number) {
				t.Days += v
				break
			}
		}
	}
	if len(t.Name) < 1 || len(t.Name) > 100 || !slugPattern.MatchString(t.Slug) || !validTimezone(t.Timezone) || e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || t.StartMin >= t.EndMin || t.Duration < 5 || t.Duration > 240 || t.Duration > t.EndMin-t.StartMin || t.Buffer < 0 || t.Buffer > 120 || t.Notice < 0 || t.Notice > 43200 || t.Horizon < 1 || t.Horizon > 90 {
		a.fail(w, r, 400, "Check the name, URL, timezone, and hours. Length: 5–240 minutes, buffer: 0–120 minutes, notice: 0–43200 minutes, booking horizon: 1–90 days.")
		return
	}
	if t.Active && t.Days == "" {
		a.fail(w, r, 400, "Choose at least one available weekday before turning this meeting type on.")
		return
	}
	u := currentUser(r)
	if t.ID == 0 {
		_, e = a.db.ExecContext(r.Context(), `INSERT INTO meeting_types(user_id,slug,name,timezone,days,start_min,end_min,duration,buffer,notice,horizon,active) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, u.ID, t.Slug, t.Name, t.Timezone, t.Days, t.StartMin, t.EndMin, t.Duration, t.Buffer, t.Notice, t.Horizon, t.Active)
	} else {
		_, e = a.db.ExecContext(r.Context(), `UPDATE meeting_types SET slug=?,name=?,timezone=?,days=?,start_min=?,end_min=?,duration=?,buffer=?,notice=?,horizon=?,active=? WHERE id=? AND user_id=?`, t.Slug, t.Name, t.Timezone, t.Days, t.StartMin, t.EndMin, t.Duration, t.Buffer, t.Notice, t.Horizon, t.Active, t.ID, u.ID)
	}
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			a.fail(w, r, 409, "You already have a meeting type at that URL.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
}
func (a *App) deleteMeetingType(w http.ResponseWriter, r *http.Request) {
	// Bookings keep their own copy of title, time, and timezone, so history survives.
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM meeting_types WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=deleted", 303)
}
func (a *App) saveCalendarSettings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	cs, e := a.calendars(r.Context(), u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	write, e := strconv.ParseInt(r.PostForm.Get("write"), 10, 64)
	if e != nil {
		a.fail(w, r, 400, "Choose a calendar for new bookings.")
		return
	}
	valid := false
	for _, c := range cs {
		if c.ID == write && (c.Role == "owner" || c.Role == "writer") {
			valid = true
		}
	}
	if !valid {
		a.fail(w, r, 400, "The destination must be a writable calendar belonging to one of your connected accounts.")
		return
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	for _, c := range cs {
		checked := c.ID == write
		for _, id := range r.PostForm["busy"] {
			if id == strconv.FormatInt(c.ID, 10) {
				checked = true
			}
		}
		if _, e = tx.ExecContext(r.Context(), "UPDATE calendars SET check_busy=? WHERE id=?", checked, c.ID); e != nil {
			a.internal(w, r, e)
			return
		}
	}
	_, e = tx.ExecContext(r.Context(), "UPDATE users SET write_calendar=? WHERE id=?", write, u.ID)
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
}
func (a *App) blockDay(w http.ResponseWriter, r *http.Request) {
	day := r.PostForm.Get("day")
	if _, e := time.Parse("2006-01-02", day); e != nil {
		a.fail(w, r, 400, "Choose a valid date.")
		return
	}
	_, e := a.db.ExecContext(r.Context(), "INSERT OR IGNORE INTO blocks(user_id,day) VALUES(?,?)", currentUser(r).ID, day)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
}
func (a *App) unblockDay(w http.ResponseWriter, r *http.Request) {
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM blocks WHERE user_id=? AND day=?", currentUser(r).ID, r.PostForm.Get("day"))
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
}
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	old, newPass := r.PostForm.Get("old_password"), r.PostForm.Get("new_password")
	if len(old) > 256 || len(newPass) < 12 || len(newPass) > 256 {
		a.fail(w, r, 400, "Password must be 12–256 characters.")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	defer func() { <-a.authSlots }()
	if !passwordMatches(u.Password, old) {
		a.fail(w, r, 403, "Current password is incorrect.")
		return
	}
	hash := passwordHash(newPass)
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(r.Context(), "UPDATE users SET password=? WHERE id=?", hash, u.ID)
	if e == nil {
		_, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=?", u.ID)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e == nil {
		e = a.loginSession(w, r, u.ID)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=password", 303)
}

func (a *App) publicUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	u, e := scanUser(a.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users WHERE slug=? AND enabled=1", r.PathValue("slug")))
	if errors.Is(e, sql.ErrNoRows) {
		http.NotFound(w, r)
		return u, false
	}
	if e != nil {
		a.internal(w, r, e)
		return u, false
	}
	return u, true
}
func (a *App) publicMeetingType(w http.ResponseWriter, r *http.Request) (User, MeetingType, bool) {
	u, ok := a.publicUser(w, r)
	if !ok {
		return u, MeetingType{}, false
	}
	t, e := scanMeetingType(a.db.QueryRowContext(r.Context(), "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE user_id=? AND slug=? AND active=1", u.ID, r.PathValue("type")))
	if errors.Is(e, sql.ErrNoRows) {
		http.NotFound(w, r)
		return u, t, false
	}
	if e != nil {
		a.internal(w, r, e)
		return u, t, false
	}
	return u, t, true
}
func (a *App) hostPage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.publicUser(w, r)
	if !ok {
		return
	}
	ts, e := a.meetingTypes(r.Context(), u.ID, true)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	if len(ts) == 1 {
		http.Redirect(w, r, "/b/"+u.Slug+"/"+ts[0].Slug, http.StatusFound)
		return
	}
	a.render(w, r, "host", Page{Title: "Meet with " + u.Name, User: u, MeetingTypes: ts, BookingURL: "/b/" + u.Slug}, 200)
}

// CalendarDay is one cell of the guest's month grid, dated in the guest's timezone.
type CalendarDay struct {
	Date                    string
	Day                     int
	InMonth, Open, Selected bool
}

const dateLayout = "2006-01-02"

func (a *App) publicPage(w http.ResponseWriter, r *http.Request) {
	u, t, ok := a.publicMeetingType(w, r)
	if !ok {
		return
	}
	host, e := time.LoadLocation(t.Timezone)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	q := r.URL.Query()
	// Without ?tz the page renders in the meeting type's zone and a script swaps in the browser's.
	guestTZ := q.Get("tz")
	detect := guestTZ == ""
	if !validTimezone(guestTZ) {
		guestTZ = t.Timezone
	}
	guest, _ := time.LoadLocation(guestTZ)
	now := time.Now()
	minDate := now.In(guest).Format(dateLayout)
	maxDate := now.In(host).AddDate(0, 0, t.Horizon).In(guest).Format(dateLayout)
	date := q.Get("date")
	var day time.Time
	if date != "" {
		day, e = time.ParseInLocation(dateLayout, date, guest)
		if e != nil || date < minDate || date > maxDate {
			a.fail(w, r, 400, "Choose a date within the booking window.")
			return
		}
	}
	month := q.Get("month")
	if month == "" {
		month = minDate[:7]
		if date != "" {
			month = date[:7]
		}
	}
	monthStart, e := time.ParseInLocation("2006-01", month, guest)
	if e != nil || month < minDate[:7] || month > maxDate[:7] {
		a.fail(w, r, 400, "Choose a month within the booking window.")
		return
	}
	monthEnd := monthStart.AddDate(0, 1, 0)
	p := Page{Title: t.Name + " with " + u.Name, User: u, MeetingType: t, Date: date, BookingURL: "/b/" + u.Slug + "/" + t.Slug, GuestTimezone: guestTZ, DetectTimezone: detect, Timezones: a.timezones, Month: month, MonthLabel: monthStart.Format("January 2006")}
	if month > minDate[:7] {
		p.PrevMonth = monthStart.AddDate(0, -1, 0).Format("2006-01")
	}
	if month < maxDate[:7] {
		p.NextMonth = monthEnd.Format("2006-01")
	}
	slots, e := a.availability(r.Context(), u, t, monthStart, monthEnd, now)
	if e != nil {
		slog.Warn("availability unavailable", "host", u.ID, "error", e)
		p.Error = "Availability could not be verified with Google. Please try again shortly."
		a.render(w, r, "booking", p, 503)
		return
	}
	open := map[string][]Slot{}
	for _, s := range slots {
		d := time.Unix(s.Start, 0).In(guest).Format(dateLayout)
		open[d] = append(open[d], s)
	}
	p.Weeks = calendarWeeks(monthStart, date, open)
	if date == "" {
		a.render(w, r, "booking", p, 200)
		return
	}
	p.SelectedLabel = day.Format("Monday, January 2")
	p.Slots = guestLabels(open[date], guest)
	if chosen := q.Get("start"); chosen != "" {
		start, e := strconv.ParseInt(chosen, 10, 64)
		if e != nil {
			a.fail(w, r, 400, "Invalid time")
			return
		}
		found := false
		for _, s := range p.Slots {
			if s.Start == start {
				found = true
				p.Start = start
				p.SlotLabel = s.Label
				p.Ticket = a.ticket(u.ID, t, start)
			}
		}
		if !found {
			p.Error = "That time is no longer available. Please choose another."
		}
		if p.Locations, e = a.locations(r.Context(), u); e != nil {
			a.internal(w, r, e)
			return
		}
	}
	a.render(w, r, "booking", p, 200)
}

// calendarWeeks lays out monthStart's month as Sunday-first weeks.
func calendarWeeks(monthStart time.Time, selected string, open map[string][]Slot) [][]CalendarDay {
	end := monthStart.AddDate(0, 1, 0)
	var weeks [][]CalendarDay
	for d := monthStart.AddDate(0, 0, -int(monthStart.Weekday())); d.Before(end); {
		week := make([]CalendarDay, 0, 7)
		for range 7 {
			key := d.Format(dateLayout)
			week = append(week, CalendarDay{Date: key, Day: d.Day(), InMonth: d.Month() == monthStart.Month(), Open: len(open[key]) > 0, Selected: key == selected})
			d = d.AddDate(0, 0, 1)
		}
		weeks = append(weeks, week)
	}
	return weeks
}

// guestLabels shows times in the guest's zone, adding the UTC offset only on a day
// whose offset changes, where a repeated hour would otherwise show twice.
func guestLabels(slots []Slot, loc *time.Location) []Slot {
	offsets := map[string]bool{}
	for _, s := range slots {
		offsets[time.Unix(s.Start, 0).In(loc).Format("-07:00")] = true
	}
	layout := "15:04"
	if len(offsets) > 1 {
		layout = "15:04 (UTC-07:00)"
	}
	out := make([]Slot, len(slots))
	for i, s := range slots {
		out[i] = Slot{Start: s.Start, Label: time.Unix(s.Start, 0).In(loc).Format(layout)}
	}
	return out
}

// A signed ticket freezes host, meeting type, slot, duration, expiry, and an idempotency key.
// The ticket MAC doubles as the unguessable manage token; retries return the same booking.
func (a *App) ticket(uid int64, t MeetingType, start int64) string {
	payload := fmt.Sprintf("%d.%d.%d.%d.%d.%s", uid, t.ID, start, t.Duration, time.Now().Add(30*time.Minute).Unix(), randomHex(16))
	return payload + "." + a.ticketMAC(payload)
}
func (a *App) ticketMAC(payload string) string {
	mac := hmac.New(sha256.New, a.signingKey)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
func (a *App) verifyTicket(s string, u User, t MeetingType) (start int64, id, token string, err error) {
	parts := strings.Split(s, ".")
	if len(parts) != 7 {
		return 0, "", "", errors.New("invalid ticket")
	}
	payload := strings.Join(parts[:6], ".")
	mac := a.ticketMAC(payload)
	if !hmac.Equal([]byte(mac), []byte(parts[6])) {
		return 0, "", "", errors.New("invalid signature")
	}
	uid, e1 := strconv.ParseInt(parts[0], 10, 64)
	tid, e2 := strconv.ParseInt(parts[1], 10, 64)
	start, e3 := strconv.ParseInt(parts[2], 10, 64)
	duration, e4 := strconv.Atoi(parts[3])
	expiry, e5 := strconv.ParseInt(parts[4], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || uid != u.ID || tid != t.ID || duration != t.Duration || expiry < time.Now().Unix() {
		return 0, "", "", errors.New("expired ticket")
	}
	return start, parts[5], mac, nil
}
func (a *App) book(w http.ResponseWriter, r *http.Request) {
	u, t, ok := a.publicMeetingType(w, r)
	if !ok {
		return
	}
	if r.PostForm.Get("website") != "" {
		a.fail(w, r, 400, "Unable to book.")
		return
	}
	start, id, token, e := a.verifyTicket(r.PostForm.Get("ticket"), u, t)
	if e != nil {
		a.fail(w, r, 400, "This booking form expired. Return to the booking page and choose a time again.")
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if len(name) < 1 || len(name) > 100 || !validEmail(email) {
		a.fail(w, r, 400, "Enter your name and a valid email address.")
		return
	}
	location, meet, e := a.chosenLocation(r, u)
	if e != nil {
		a.fail(w, r, 400, "Choose where to meet, or enter a location up to 500 characters.")
		return
	}
	if existing, e := a.getBooking(r.Context(), token); e == nil {
		http.Redirect(w, r, "/manage/"+existing.ManageToken, 303)
		return
	} else if !errors.Is(e, sql.ErrNoRows) {
		a.internal(w, r, e)
		return
	}
	guestTZ := r.PostForm.Get("tz")
	if !validTimezone(guestTZ) {
		guestTZ = t.Timezone
	}
	slots, e := a.availability(r.Context(), u, t, time.Unix(start, 0), time.Unix(start+1, 0), time.Now())
	if e != nil {
		a.fail(w, r, 503, "Could not verify availability with Google. Please try again.")
		return
	}
	available := false
	for _, s := range slots {
		if s.Start == start {
			available = true
		}
	}
	if !available {
		a.fail(w, r, 409, "That time is no longer available. Please choose another slot.")
		return
	}
	b := Booking{ID: id, UserID: u.ID, CalendarID: u.WriteCalendar.Int64, GuestName: name, GuestEmail: email, Start: start, End: start + int64(t.Duration*60), BlockStart: start - int64(t.Buffer*60), BlockEnd: start + int64((t.Duration+t.Buffer)*60), Title: t.Name + ": " + name + " / " + u.Name, Location: location, Meet: meet, Timezone: t.Timezone, GuestTimezone: guestTZ, ManageToken: token, Status: "pending", Created: time.Now().Unix()}
	if e = a.reserve(r.Context(), b); e != nil {
		if existing, err := a.getBooking(r.Context(), token); err == nil {
			http.Redirect(w, r, "/manage/"+existing.ManageToken, 303)
			return
		}
		if strings.Contains(e.Error(), "slot_overlap") {
			a.fail(w, r, 409, "Someone just booked this time. Please choose another slot.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	// Respond quickly. The durable worker confirms with Google and sends its invitation.
	http.Redirect(w, r, "/manage/"+token, 303)
}

// chosenLocation resolves the guest's pick. Their own text wins over the selected option,
// so typing a location works without script to deselect the preselected default.
func (a *App) chosenLocation(r *http.Request, u User) (text string, meet bool, err error) {
	text = strings.TrimSpace(r.PostForm.Get("custom_location"))
	if len(text) > 500 {
		return "", false, errors.New("custom location too long")
	}
	choice := r.PostForm.Get("location")
	if text != "" || choice == "" {
		return text, false, nil
	}
	var l Location
	e := a.db.QueryRowContext(r.Context(), "SELECT kind,label,detail FROM locations WHERE id=? AND user_id=?", choice, u.ID).Scan(&l.Kind, &l.Label, &l.Detail)
	if e != nil {
		return "", false, fmt.Errorf("location %q: %w", choice, e)
	}
	if l.Kind == "meet" {
		// The link does not exist until Google creates the event.
		return "", true, nil
	}
	return l.Text(), false, nil
}
func (a *App) manage(w http.ResponseWriter, r *http.Request) {
	b, e := a.getBooking(r.Context(), r.PathValue("token"))
	if errors.Is(e, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	u, e := a.userByID(r.Context(), b.UserID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	a.render(w, r, "manage", Page{Title: "Your booking", Booking: b, User: u}, 200)
}
func (a *App) cancelPublic(w http.ResponseWriter, r *http.Request) {
	b, e := a.getBooking(r.Context(), r.PathValue("token"))
	if errors.Is(e, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	if e = a.cancelBooking(r.Context(), b); e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/manage/"+b.ManageToken, 303)
}
func (a *App) ownedBooking(r *http.Request) (Booking, error) {
	return scanBooking(a.db.QueryRowContext(r.Context(), "SELECT "+bookingColumns+" FROM bookings WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID))
}
func (a *App) cancelAdmin(w http.ResponseWriter, r *http.Request) {
	b, e := a.ownedBooking(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	if e = a.cancelBooking(r.Context(), b); e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=cancelled", 303)
}
func (a *App) retryAdmin(w http.ResponseWriter, r *http.Request) {
	b, e := a.ownedBooking(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	a.syncBooking(r.Context(), b.ID)
	http.Redirect(w, r, "/?notice=retry", 303)
}
func icsEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, ";", "\\;")
	return strings.ReplaceAll(s, ",", "\\,")
}
func icsFold(s string) string {
	var out strings.Builder
	n := 0
	for _, r := range s {
		v := string(r)
		if n+len(v) > 75 {
			out.WriteString("\r\n ")
			n = 1
		}
		out.WriteString(v)
		n += len(v)
	}
	return out.String() + "\r\n"
}
func (a *App) ics(w http.ResponseWriter, r *http.Request) {
	b, e := a.getBooking(r.Context(), r.PathValue("token"))
	if e != nil || b.Status != "confirmed" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="booking.ics"`)
	format := func(n int64) string { return time.Unix(n, 0).UTC().Format("20060102T150405Z") }
	for _, line := range []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Slot//Booking//EN", "BEGIN:VEVENT", "UID:" + b.ID + "@slot", "DTSTAMP:" + format(b.Created), "DTSTART:" + format(b.Start), "DTEND:" + format(b.End), "SUMMARY:" + icsEscape(b.Title), "LOCATION:" + icsEscape(b.Location), "URL:" + a.cfg.PublicURL + "/manage/" + b.ManageToken, "END:VEVENT", "END:VCALENDAR"} {
		fmt.Fprint(w, icsFold(line))
	}
}
