package slot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	_, movable, e := a.rescheduleType(r.Context(), b)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	a.render(w, r, "manage", Page{Title: "Your booking", Booking: b, User: u, CanReschedule: movable}, http.StatusOK)
}

// rescheduleType returns the meeting type whose hours b may move within, and whether
// the guest may move it now. Until the host can approve a move, a booking needing
// approval moves only while it is still a request.
func (a *App) rescheduleType(ctx context.Context, b Booking) (MeetingType, bool, error) {
	upcoming := b.Start > time.Now().Unix() && (b.Status == "requested" || b.Status == "pending" || b.Status == "confirmed")
	if !upcoming || !b.MeetingTypeID.Valid {
		return MeetingType{}, false, nil
	}
	t, e := scanMeetingType(a.db.QueryRowContext(ctx, "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE id=? AND user_id=? AND active=1", b.MeetingTypeID.Int64, b.UserID))
	if errors.Is(e, sql.ErrNoRows) {
		return t, false, nil
	}
	if e != nil {
		return t, false, e
	}
	return t, !t.Approval || b.Status == "requested", nil
}

// movableBooking loads the booking named in the URL, with its host and meeting type,
// and answers the request itself when the guest may not move it.
func (a *App) movableBooking(w http.ResponseWriter, r *http.Request) (Booking, User, MeetingType, bool) {
	b, e := a.getBooking(r.Context(), r.PathValue("token"))
	if errors.Is(e, sql.ErrNoRows) {
		http.NotFound(w, r)
		return b, User{}, MeetingType{}, false
	}
	if e != nil {
		a.internal(w, r, e)
		return b, User{}, MeetingType{}, false
	}
	t, movable, e := a.rescheduleType(r.Context(), b)
	if e != nil {
		a.internal(w, r, e)
		return b, User{}, t, false
	}
	if !movable {
		a.fail(w, r, http.StatusConflict, "This booking can no longer be rescheduled here. Cancel it and book a new time instead.")
		return b, User{}, t, false
	}
	u, e := a.userByID(r.Context(), b.UserID)
	if e != nil {
		a.internal(w, r, e)
		return b, u, t, false
	}
	return b, u, t, true
}

func (a *App) reschedulePage(w http.ResponseWriter, r *http.Request) {
	if b, u, t, ok := a.movableBooking(w, r); ok {
		a.showBooking(w, r, u, t, "/manage/"+b.ManageToken+"/reschedule", b)
	}
}

func (a *App) reschedule(w http.ResponseWriter, r *http.Request) {
	b, u, t, ok := a.movableBooking(w, r)
	if !ok {
		return
	}
	start, _, _, e := a.verifyTicket(r.PostForm.Get("ticket"), u, t)
	if e != nil {
		a.fail(w, r, http.StatusBadRequest, "This form expired. Return to your booking and choose a new time again.")
		return
	}
	// A repeated submit finds the booking already moved.
	if start == b.Start {
		http.Redirect(w, r, "/manage/"+b.ManageToken, http.StatusSeeOther)
		return
	}
	slots, e := a.availability(r.Context(), u, t, time.Unix(start, 0), time.Unix(start+1, 0), time.Now(), b)
	if e != nil {
		a.fail(w, r, http.StatusServiceUnavailable, "Could not verify availability with Google. Please try again.")
		return
	}
	if !slices.ContainsFunc(slots, func(s Slot) bool { return s.Start == start }) {
		a.fail(w, r, http.StatusConflict, "That time is no longer available. Please choose another.")
		return
	}
	if e = a.moveBooking(r.Context(), b, t, start); errors.Is(e, errNotMoved) {
		a.fail(w, r, http.StatusConflict, "Your booking kept its time: the new one was just taken, or the booking changed. Please check it and try again.")
		return
	} else if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/manage/"+b.ManageToken, http.StatusSeeOther)
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

// approveAdmin hands a request to the worker, which creates the event and invites the guest.
func (a *App) approveAdmin(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "UPDATE bookings SET status='pending',next_attempt=0 WHERE id=? AND user_id=? AND status='requested' AND start>?", "approved")
}

func (a *App) declineAdmin(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "UPDATE bookings SET status='declined' WHERE id=? AND user_id=? AND status='requested' AND start>?", "declined")
}

// decide applies the host's answer to one of their upcoming requests.
func (a *App) decide(w http.ResponseWriter, r *http.Request, query, notice string) {
	res, e := a.db.ExecContext(r.Context(), query, r.PathValue("id"), currentUser(r).ID, time.Now().Unix())
	if e != nil {
		a.internal(w, r, e)
		return
	}
	if n, e := res.RowsAffected(); e != nil || n == 0 {
		a.fail(w, r, http.StatusConflict, "Only upcoming requests can be approved or declined. The guest may have cancelled.")
		return
	}
	http.Redirect(w, r, a.adminURL("/?notice="+notice), http.StatusSeeOther)
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
