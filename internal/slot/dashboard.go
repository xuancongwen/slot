package slot

import (
	"database/sql"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Account struct {
	ID       int64
	Identity string
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
	p.Accounts, e = queryAll(r.Context(), a.db, func(s scanner) (Account, error) {
		var ac Account
		return ac, s.Scan(&ac.ID, &ac.Identity)
	}, "SELECT id,identity FROM accounts WHERE user_id=? ORDER BY id", u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	p.Blocks, e = queryAll(r.Context(), a.db, scanString, "SELECT day FROM blocks WHERE user_id=? ORDER BY day", u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	notices := map[string]string{"saved": "Your settings are saved.", "connected": "Google account connected. Choose which calendars to check and where bookings should go.", "refreshed": "Calendar list refreshed.", "cancelled": "Cancellation requested. The slot stays reserved until Google confirms.", "retry": "Calendar sync retried.", "password": "Password changed. Other sessions have been signed out.", "deleted": "Meeting type deleted."}
	p.Notice = notices[r.URL.Query().Get("notice")]
	a.render(w, r, "dashboard", p, http.StatusOK)
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if len(name) < 1 || len(name) > 100 {
		a.fail(w, r, http.StatusBadRequest, "Use a display name up to 100 characters.")
		return
	}
	enabled := r.PostForm.Get("enabled") == "on"
	if enabled && !u.WriteCalendar.Valid {
		a.fail(w, r, http.StatusBadRequest, "Select a destination calendar before publishing.")
		return
	}
	_, e := a.db.ExecContext(r.Context(), `UPDATE users SET name=?,enabled=? WHERE id=?`, name, enabled, u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
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
		a.fail(w, r, http.StatusBadRequest, "Choose a calendar for new bookings.")
		return
	}
	if !slices.ContainsFunc(cs, func(c Calendar) bool { return c.ID == write && c.Writable() }) {
		a.fail(w, r, http.StatusBadRequest, "The destination must be a writable calendar belonging to one of your connected accounts.")
		return
	}
	e = a.inTx(r.Context(), func(tx *sql.Tx) error {
		for _, c := range cs {
			checked := c.ID == write || slices.Contains(r.PostForm["busy"], strconv.FormatInt(c.ID, 10))
			if _, e := tx.ExecContext(r.Context(), "UPDATE calendars SET check_busy=? WHERE id=?", checked, c.ID); e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(r.Context(), "UPDATE users SET write_calendar=? WHERE id=?", write, u.ID)
		return e
	})
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
}

func (a *App) blockDay(w http.ResponseWriter, r *http.Request) {
	day := r.PostForm.Get("day")
	if _, e := time.Parse(dateLayout, day); e != nil {
		a.fail(w, r, http.StatusBadRequest, "Choose a valid date.")
		return
	}
	_, e := a.db.ExecContext(r.Context(), "INSERT OR IGNORE INTO blocks(user_id,day) VALUES(?,?)", currentUser(r).ID, day)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
}

func (a *App) unblockDay(w http.ResponseWriter, r *http.Request) {
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM blocks WHERE user_id=? AND day=?", currentUser(r).ID, r.PostForm.Get("day"))
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
}
