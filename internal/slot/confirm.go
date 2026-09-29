package slot

import (
	"context"
	"crypto/hmac"
	"fmt"
	"net/http"
	"time"
)

// A guest confirms a booking from an emailed link before Google invites them, so
// the form cannot be used to send invitations to someone else's address.
const (
	// holdWindow is how long an unconfirmed booking holds its time.
	holdWindow = 30 * time.Minute
	// maxUnconfirmed caps confirmation emails to one address per day.
	maxUnconfirmed = 3
)

// HoldExpires is when an unconfirmed booking releases its time.
func (b Booking) HoldExpires() int64 { return b.Created + int64(holdWindow/time.Second) }

// confirmCode proves the guest read the email. The booking page shows only the
// manage token, so the booker's browser never sees this code.
func (a *App) confirmCode(b Booking) string { return a.ticketMAC("confirm." + b.ID) }

func (a *App) sendConfirmation(ctx context.Context, u User, t MeetingType, b Booking) error {
	loc, e := time.LoadLocation(b.GuestTimezone)
	if e != nil {
		loc = time.UTC
	}
	link := a.cfg.PublicURL + "/manage/" + b.ManageToken + "?confirm=" + a.confirmCode(b)
	// Nothing the guest typed goes in the email, or the form could carry spam to the address.
	body := "To book " + t.Name + " with " + u.Name + " on " + time.Unix(b.Start, 0).In(loc).Format("Monday, January 2 at 15:04 MST") + ", confirm within " + fmt.Sprint(holdWindow.Minutes()) + " minutes:\n\n" +
		link + "\n\n" +
		"The time is held for you until then. If you didn't ask for this, ignore this email and the time will be released.\n"
	return a.mail.Send(ctx, b.GuestEmail, "Confirm your booking with "+u.Name, body)
}

// confirmPublic accepts the code from the emailed link. It is a POST behind a
// button, because mail scanners open links in emails on their own.
func (a *App) confirmPublic(w http.ResponseWriter, r *http.Request) {
	b, ok := a.publicBooking(w, r)
	if !ok {
		return
	}
	if !hmac.Equal([]byte(r.PostForm.Get("code")), []byte(a.confirmCode(b))) {
		a.fail(w, r, http.StatusBadRequest, "This confirmation link is not valid. Open the link from your email again.")
		return
	}
	res, e := a.db.ExecContext(r.Context(), "UPDATE bookings SET verified=1,next_attempt=0 WHERE id=? AND verified=0 AND status IN ('requested','pending') AND created>?", b.ID, time.Now().Add(-holdWindow).Unix())
	if e != nil {
		a.internal(w, r, e)
		return
	}
	if n, e := res.RowsAffected(); e != nil || (n == 0 && !b.Verified) {
		a.fail(w, r, http.StatusConflict, "This booking was not confirmed in time, so its time was released. Please book again.")
		return
	}
	http.Redirect(w, r, "/manage/"+b.ManageToken, http.StatusSeeOther)
}
