package slot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) publishedHost(ctx context.Context, slug string) (User, error) {
	return scanUser(a.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE slug=? AND enabled=1", slug))
}

func (a *App) publicUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	u, e := a.publishedHost(r.Context(), r.PathValue("slug"))
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

// home serves the single host's page at the root. Until that host publishes, and on
// servers with several hosts, it is a generic landing page.
func (a *App) home(w http.ResponseWriter, r *http.Request) {
	if a.cfg.SingleHost != "" {
		u, e := a.publishedHost(r.Context(), a.cfg.SingleHost)
		if e == nil {
			a.showHost(w, r, u, "/")
			return
		}
		if !errors.Is(e, sql.ErrNoRows) {
			a.internal(w, r, e)
			return
		}
	}
	a.render(w, r, "home", Page{Title: "Home"}, http.StatusOK)
}

func (a *App) hostPage(w http.ResponseWriter, r *http.Request) {
	if u, ok := a.publicUser(w, r); ok {
		a.showHost(w, r, u, "/b/"+u.Slug)
	}
}

// showHost lists the host's meeting types, or shows the calendar right at viewURL
// when there is only one, rather than redirecting to the type's own URL.
func (a *App) showHost(w http.ResponseWriter, r *http.Request, u User, viewURL string) {
	ts, e := a.meetingTypes(r.Context(), u.ID, true)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	if len(ts) == 1 {
		a.showBooking(w, r, u, ts[0], viewURL, Booking{})
		return
	}
	a.render(w, r, "host", Page{Title: "Meet with " + u.Name, User: u, MeetingTypes: ts, BookingURL: "/b/" + u.Slug}, http.StatusOK)
}

// CalendarDay is one cell of the guest's month grid, dated in the guest's timezone.
type CalendarDay struct {
	Date                    string
	Day                     int
	InMonth, Open, Selected bool
}

func (a *App) publicPage(w http.ResponseWriter, r *http.Request) {
	if u, t, ok := a.publicMeetingType(w, r); ok {
		a.showBooking(w, r, u, t, "/b/"+u.Slug+"/"+t.Slug, Booking{})
	}
}

// showBooking renders t's calendar at viewURL. The booking form always posts to the
// type's own URL, so the same POST handler serves every place the calendar appears.
// When a guest reschedules, moving is their booking and the form moves it instead.
func (a *App) showBooking(w http.ResponseWriter, r *http.Request, u User, t MeetingType, viewURL string, moving Booking) {
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
			a.fail(w, r, http.StatusBadRequest, "Choose a date within the booking window.")
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
		a.fail(w, r, http.StatusBadRequest, "Choose a month within the booking window.")
		return
	}
	monthEnd := monthStart.AddDate(0, 1, 0)
	p := Page{Title: t.Name + " with " + u.Name, User: u, MeetingType: t, Date: date, BookingURL: "/b/" + u.Slug + "/" + t.Slug, ViewURL: viewURL, GuestTimezone: guestTZ, DetectTimezone: detect, Analytics: true, Timezones: a.timezones, Month: month, MonthLabel: monthStart.Format("January 2006")}
	if moving.ID != "" {
		// The page's URL holds the manage token, so it is kept from the tracker.
		p.Title, p.Booking, p.BookingURL, p.Analytics = "Reschedule "+t.Name+" with "+u.Name, moving, viewURL, false
	}
	if month > minDate[:7] {
		p.PrevMonth = monthStart.AddDate(0, -1, 0).Format("2006-01")
	}
	if month < maxDate[:7] {
		p.NextMonth = monthEnd.Format("2006-01")
	}
	slots, e := a.availability(r.Context(), u, t, monthStart, monthEnd, now, moving)
	if e != nil {
		slog.Warn("availability unavailable", "host", u.ID, "error", e)
		p.Error = "Availability could not be verified with Google. Please try again shortly."
		a.render(w, r, "booking", p, http.StatusServiceUnavailable)
		return
	}
	open := map[string][]Slot{}
	for _, s := range slots {
		d := time.Unix(s.Start, 0).In(guest).Format(dateLayout)
		open[d] = append(open[d], s)
	}
	p.Weeks = calendarWeeks(monthStart, date, open)
	if date == "" {
		a.render(w, r, "booking", p, http.StatusOK)
		return
	}
	p.SelectedLabel = day.Format("Monday, January 2")
	p.Slots = guestLabels(open[date], guest)
	if chosen := q.Get("start"); chosen != "" {
		start, e := strconv.ParseInt(chosen, 10, 64)
		if e != nil {
			a.fail(w, r, http.StatusBadRequest, "Invalid time")
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
		ls, e := a.typeLocations(r.Context(), u, t)
		if e != nil {
			a.internal(w, r, e)
			return
		}
		p.Locations = offeredLocations(ls)
	}
	a.render(w, r, "booking", p, http.StatusOK)
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
		a.fail(w, r, http.StatusBadRequest, "Unable to book.")
		return
	}
	start, id, token, e := a.verifyTicket(r.PostForm.Get("ticket"), u, t)
	if e != nil {
		a.fail(w, r, http.StatusBadRequest, "This booking form expired. Return to the booking page and choose a time again.")
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if len(name) < 1 || len(name) > 100 || !validEmail(email) {
		a.fail(w, r, http.StatusBadRequest, "Enter your name and a valid email address.")
		return
	}
	if a.disposableEmail(email) {
		a.fail(w, r, http.StatusBadRequest, "Use an email address you keep. Disposable inboxes can’t book.")
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if len(reason) > 1000 {
		a.fail(w, r, http.StatusBadRequest, "Keep the reason for meeting to 1000 characters.")
		return
	}
	location, meet, e := a.chosenLocation(r, u, t)
	if e != nil {
		a.fail(w, r, http.StatusBadRequest, "Choose where to meet, or enter a location up to 500 characters.")
		return
	}
	if existing, e := a.getBooking(r.Context(), token); e == nil {
		http.Redirect(w, r, "/manage/"+existing.ManageToken, http.StatusSeeOther)
		return
	} else if !errors.Is(e, sql.ErrNoRows) {
		a.internal(w, r, e)
		return
	}
	guestTZ := r.PostForm.Get("tz")
	if !validTimezone(guestTZ) {
		guestTZ = t.Timezone
	}
	slots, e := a.availability(r.Context(), u, t, time.Unix(start, 0), time.Unix(start+1, 0), time.Now(), Booking{})
	if e != nil {
		a.fail(w, r, http.StatusServiceUnavailable, "Could not verify availability with Google. Please try again.")
		return
	}
	available := false
	for _, s := range slots {
		if s.Start == start {
			available = true
		}
	}
	if !available {
		a.fail(w, r, http.StatusConflict, "That time is no longer available. Please choose another slot.")
		return
	}
	status := "pending"
	if t.Approval {
		status = "requested"
	}
	b := Booking{ID: id, UserID: u.ID, CalendarID: u.WriteCalendar.Int64, GuestName: name, GuestEmail: email, Start: start, End: start + int64(t.Duration*60), BlockStart: start - int64(t.Buffer*60), BlockEnd: start + int64((t.Duration+t.Buffer)*60), Title: t.Name + ": " + name + " / " + u.Name, Location: location, Meet: meet, Timezone: t.Timezone, GuestTimezone: guestTZ, Reason: reason, ManageToken: token, Status: status, Created: time.Now().Unix(), MeetingTypeID: sql.NullInt64{Int64: t.ID, Valid: true}}
	if e = a.reserve(r.Context(), b); e != nil {
		if existing, err := a.getBooking(r.Context(), token); err == nil {
			http.Redirect(w, r, "/manage/"+existing.ManageToken, http.StatusSeeOther)
			return
		}
		if strings.Contains(e.Error(), "slot_overlap") {
			a.fail(w, r, http.StatusConflict, "Someone just booked this time. Please choose another slot.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	// Respond quickly. The durable worker confirms with Google and sends its invitation.
	http.Redirect(w, r, "/manage/"+token, http.StatusSeeOther)
}
