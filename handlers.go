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
	Slots                                               []Slot
	Date, MinDate, MaxDate, BookingURL, PublicURL       string
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
		a.render(w, r, "home", Page{Title: "A little space to meet"}, 200)
	})
	m.HandleFunc("GET /b/{slug}", a.publicPage)
	m.HandleFunc("POST /b/{slug}", a.book)
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
	title := "Welcome back"
	if reg {
		title = "Make time for good conversations"
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
	res, e := a.db.ExecContext(r.Context(), "INSERT INTO users(email,password,name,slug,created) VALUES(?,?,?,?,?)", email, hash, name, slug, time.Now().Unix())
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			a.fail(w, r, 409, "That email or booking URL is already registered.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	id, e := res.LastInsertId()
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
		a.render(w, r, "auth", Page{Title: "Welcome back", Admin: true, RegistrationOpen: a.cfg.RegistrationOpen, Error: "Email or password is incorrect."}, 401)
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
	p := Page{Title: "Your time, on your terms", Admin: true, User: u, Days: days, GoogleConfigured: a.cfg.GoogleClientID != "", BookingURL: a.cfg.PublicURL + "/b/" + u.Slug}
	var e error
	p.Calendars, e = a.calendars(r.Context(), u.ID)
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
	notices := map[string]string{"saved": "Your settings are saved.", "connected": "Google account connected. Choose which calendars to check and where bookings should go.", "refreshed": "Calendar list refreshed.", "cancelled": "Cancellation requested. The slot stays reserved until Google confirms.", "retry": "Calendar sync retried.", "password": "Password changed. Other sessions have been signed out."}
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
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f := r.PostForm
	name := strings.TrimSpace(f.Get("name"))
	tz := f.Get("timezone")
	_, tzerr := time.LoadLocation(tz)
	start, e1 := parseMinutes(f.Get("start"))
	end, e2 := parseMinutes(f.Get("end"))
	duration, e3 := strconv.Atoi(f.Get("duration"))
	buffer, e4 := strconv.Atoi(f.Get("buffer"))
	notice, e5 := strconv.Atoi(f.Get("notice"))
	horizon, e6 := strconv.Atoi(f.Get("horizon"))
	location := strings.TrimSpace(f.Get("location"))
	if len(name) < 1 || len(name) > 100 || tz == "" || tz == "Local" || tzerr != nil || e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || start >= end || duration < 5 || duration > 240 || duration > end-start || buffer < 0 || buffer > 120 || notice < 0 || notice > 43200 || horizon < 1 || horizon > 90 || len(location) > 500 {
		a.fail(w, r, 400, "Check the timezone and hours. Duration: 5–240 minutes, buffer: 0–120 minutes, notice: 0–43200 minutes, booking horizon: 1–90 days.")
		return
	}
	dayset := ""
	for _, d := range days {
		for _, v := range f["days"] {
			if v == strconv.Itoa(d.Number) {
				dayset += v
				break
			}
		}
	}
	enabled := f.Get("enabled") == "on"
	if enabled && (!u.WriteCalendar.Valid || dayset == "") {
		a.fail(w, r, 400, "Select a destination calendar and at least one available weekday before publishing.")
		return
	}
	_, e := a.db.ExecContext(r.Context(), `UPDATE users SET name=?,timezone=?,days=?,start_min=?,end_min=?,duration=?,buffer=?,notice=?,horizon=?,location=?,enabled=? WHERE id=?`, name, tz, dayset, start, end, duration, buffer, notice, horizon, location, enabled, u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", 303)
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
func (a *App) publicPage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.publicUser(w, r)
	if !ok {
		return
	}
	loc, e := time.LoadLocation(u.Timezone)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	now := time.Now()
	date := r.URL.Query().Get("date")
	if date == "" {
		date = now.In(loc).Format("2006-01-02")
	}
	day, e := time.ParseInLocation("2006-01-02", date, loc)
	minDate, maxDate := now.In(loc).Format("2006-01-02"), now.In(loc).AddDate(0, 0, u.Horizon).Format("2006-01-02")
	if e != nil || date < minDate || date > maxDate {
		a.fail(w, r, 400, "Choose a date within the booking window.")
		return
	}
	p := Page{Title: "Meet with " + u.Name, User: u, Date: date, MinDate: minDate, MaxDate: maxDate, BookingURL: "/b/" + u.Slug}
	p.Slots, e = a.availability(r.Context(), u, day, now)
	if e != nil {
		slog.Warn("availability unavailable", "host", u.ID, "error", e)
		p.Error = "Availability could not be verified with Google. Please try again shortly."
		a.render(w, r, "booking", p, 503)
		return
	}
	if chosen := r.URL.Query().Get("start"); chosen != "" {
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
				p.Ticket = a.ticket(u.ID, start, u.Duration)
			}
		}
		if !found {
			p.Error = "That time is no longer available. Please choose another."
		}
	}
	a.render(w, r, "booking", p, 200)
}

// A signed ticket freezes host, slot, duration, expiry, and an idempotency key.
// The ticket MAC doubles as the unguessable manage token; retries return the same booking.
func (a *App) ticket(uid, start int64, duration int) string {
	payload := fmt.Sprintf("%d.%d.%d.%d.%s", uid, start, duration, time.Now().Add(30*time.Minute).Unix(), randomHex(16))
	return payload + "." + a.ticketMAC(payload)
}
func (a *App) ticketMAC(payload string) string {
	mac := hmac.New(sha256.New, a.signingKey)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
func (a *App) verifyTicket(s string, u User) (start int64, id, token string, err error) {
	parts := strings.Split(s, ".")
	if len(parts) != 6 {
		return 0, "", "", errors.New("invalid ticket")
	}
	payload := strings.Join(parts[:5], ".")
	mac := a.ticketMAC(payload)
	if !hmac.Equal([]byte(mac), []byte(parts[5])) {
		return 0, "", "", errors.New("invalid signature")
	}
	uid, e1 := strconv.ParseInt(parts[0], 10, 64)
	start, e2 := strconv.ParseInt(parts[1], 10, 64)
	duration, e3 := strconv.Atoi(parts[2])
	expiry, e4 := strconv.ParseInt(parts[3], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || uid != u.ID || duration != u.Duration || expiry < time.Now().Unix() {
		return 0, "", "", errors.New("expired ticket")
	}
	return start, parts[4], mac, nil
}
func (a *App) book(w http.ResponseWriter, r *http.Request) {
	u, ok := a.publicUser(w, r)
	if !ok {
		return
	}
	if r.PostForm.Get("website") != "" {
		a.fail(w, r, 400, "Unable to book.")
		return
	}
	start, id, token, e := a.verifyTicket(r.PostForm.Get("ticket"), u)
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
	if existing, e := a.getBooking(r.Context(), token); e == nil {
		http.Redirect(w, r, "/manage/"+existing.ManageToken, 303)
		return
	} else if !errors.Is(e, sql.ErrNoRows) {
		a.internal(w, r, e)
		return
	}
	loc, e := time.LoadLocation(u.Timezone)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	t := time.Unix(start, 0).In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	slots, e := a.availability(r.Context(), u, day, time.Now())
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
	b := Booking{ID: id, UserID: u.ID, CalendarID: u.WriteCalendar.Int64, GuestName: name, GuestEmail: email, Start: start, End: start + int64(u.Duration*60), BlockStart: start - int64(u.Buffer*60), BlockEnd: start + int64((u.Duration+u.Buffer)*60), Title: name + " / " + u.Name, Location: u.Location, Timezone: u.Timezone, ManageToken: token, Status: "pending", Created: time.Now().Unix()}
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
