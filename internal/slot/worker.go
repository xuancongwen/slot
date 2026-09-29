package slot

import (
	"context"
	"log/slog"
	"time"
)

// checkInterval is how often each upcoming confirmed booking is compared with Google.
const checkInterval = 5 * time.Minute

func (a *App) worker(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		a.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) reconcile(ctx context.Context) {
	ids, e := queryAll(ctx, a.db, scanString, "SELECT id FROM bookings WHERE status IN ('pending','cancel_pending') AND next_attempt<=? ORDER BY created LIMIT 20", time.Now().Unix())
	if e != nil {
		slog.Error("list bookings to sync", "error", e)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		a.syncBooking(ctx, id)
	}
	now := time.Now()
	ids, e = queryAll(ctx, a.db, scanString, "SELECT id FROM bookings WHERE status='confirmed' AND end>? AND checked<=? ORDER BY checked LIMIT 20", now.Unix(), now.Add(-checkInterval).Unix())
	if e != nil {
		slog.Error("list bookings to check", "error", e)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		a.checkBooking(ctx, id)
	}
	for _, table := range []string{"sessions", "oauth_states"} {
		if _, e = a.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE expires<?", time.Now().Unix()); e != nil && ctx.Err() == nil {
			slog.Error("expire rows", "table", table, "error", e)
		}
	}
}

func (a *App) syncBooking(ctx context.Context, id string) {
	// Serialize Google writes and cancellation transitions within this single-process app.
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	b, e := scanBooking(a.db.QueryRowContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE id=?", id))
	if e != nil || (b.Status != "pending" && b.Status != "cancel_pending") {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	c, e := a.bookingCalendar(ctx, b.CalendarID)
	status := "confirmed"
	location := b.Location
	if e == nil {
		if b.Status == "cancel_pending" {
			status = "cancelled"
			e = a.google.Delete(ctx, c, b)
		} else {
			var meetLink string
			meetLink, e = a.google.Insert(ctx, c, b)
			if meetLink != "" {
				location = meetLink
			}
		}
	}
	if e != nil {
		delay := min(3600, 15*(1<<min(b.Attempts, 8)))
		_, dbErr := a.db.ExecContext(ctx, "UPDATE bookings SET attempts=attempts+1,next_attempt=?,last_error=? WHERE id=?", time.Now().Add(time.Duration(delay)*time.Second).Unix(), e.Error(), id)
		slog.Warn("calendar sync pending", "booking", id, "error", e, "database_error", dbErr)
		return
	}
	// The event was just written, so its first check can wait a full interval.
	if _, e = a.db.ExecContext(ctx, "UPDATE bookings SET status=?,location=?,last_error='',next_attempt=0,checked=? WHERE id=?", status, location, time.Now().Unix(), id); e != nil {
		slog.Error("persist calendar result", "booking", id, "error", e)
	}
}

// checkBooking applies changes made to a confirmed booking's event in Google Calendar.
// A deleted event cancels the booking. A guest who declines has the event removed, which
// frees the time on the destination calendar. A moved event moves the booking and its buffers.
func (a *App) checkBooking(ctx context.Context, id string) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	b, e := scanBooking(a.db.QueryRowContext(ctx, "SELECT "+bookingColumns+" FROM bookings WHERE id=?", id))
	if e != nil || b.Status != "confirmed" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	c, e := a.bookingCalendar(ctx, b.CalendarID)
	var event EventState
	if e == nil {
		event, e = a.google.Check(ctx, c, b)
	}
	now := time.Now().Unix()
	if e != nil {
		_, dbErr := a.db.ExecContext(ctx, "UPDATE bookings SET checked=?,last_error=? WHERE id=?", now, e.Error(), id)
		slog.Warn("calendar check failed", "booking", id, "error", e, "database_error", dbErr)
		return
	}
	status := b.Status
	switch {
	case event.Gone:
		status = "cancelled"
	case event.Declined:
		status = "cancel_pending"
	}
	if event.Start != 0 && (event.Start != b.Start || event.End != b.End) {
		b.BlockStart, b.BlockEnd = event.Start-(b.Start-b.BlockStart), event.End+(b.BlockEnd-b.End)
		b.Start, b.End = event.Start, event.End
	}
	if _, e = a.db.ExecContext(ctx, "UPDATE bookings SET status=?,start=?,end=?,block_start=?,block_end=?,checked=?,last_error='',next_attempt=0 WHERE id=?", status, b.Start, b.End, b.BlockStart, b.BlockEnd, now, id); e != nil {
		slog.Error("persist calendar check", "booking", id, "error", e)
	}
}

func (a *App) cancelBooking(ctx context.Context, b Booking) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	_, e := a.db.ExecContext(ctx, "UPDATE bookings SET status='cancel_pending',next_attempt=0 WHERE id=? AND status IN ('pending','confirmed')", b.ID)
	return e
}
