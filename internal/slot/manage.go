package slot

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
	a.render(w, r, "manage", Page{Title: "Your booking", Booking: b, User: u}, http.StatusOK)
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
	http.Redirect(w, r, "/manage/"+b.ManageToken, http.StatusSeeOther)
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
	http.Redirect(w, r, a.adminURL("/?notice=cancelled"), http.StatusSeeOther)
}

func (a *App) retryAdmin(w http.ResponseWriter, r *http.Request) {
	b, e := a.ownedBooking(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	a.syncBooking(r.Context(), b.ID)
	http.Redirect(w, r, a.adminURL("/?notice=retry"), http.StatusSeeOther)
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
	for _, line := range []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Slot//Booking//EN", "BEGIN:VEVENT", "UID:" + b.ID + "@slot", "DTSTAMP:" + format(b.Created), "DTSTART:" + format(b.Start), "DTEND:" + format(b.End), "SUMMARY:" + icsEscape(b.Title), "LOCATION:" + icsEscape(b.Location), "DESCRIPTION:" + icsEscape(b.Reason), "URL:" + a.cfg.PublicURL + "/manage/" + b.ManageToken, "END:VEVENT", "END:VCALENDAR"} {
		fmt.Fprint(w, icsFold(line))
	}
}
