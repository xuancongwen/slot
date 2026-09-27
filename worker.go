package main

import (
	"context"
	"log/slog"
	"time"
)

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
	rows, e := a.db.QueryContext(ctx, "SELECT id FROM bookings WHERE status IN ('pending','cancel_pending') AND next_attempt<=? ORDER BY created LIMIT 20", time.Now().Unix())
	if e != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		a.syncBooking(ctx, id)
	}
	a.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires<?", time.Now().Unix())
	a.db.ExecContext(ctx, "DELETE FROM oauth_states WHERE expires<?", time.Now().Unix())
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
	if _, e = a.db.ExecContext(ctx, "UPDATE bookings SET status=?,location=?,last_error='',next_attempt=0 WHERE id=?", status, location, id); e != nil {
		slog.Error("persist calendar result", "booking", id, "error", e)
	}
}
func (a *App) cancelBooking(ctx context.Context, b Booking) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	_, e := a.db.ExecContext(ctx, "UPDATE bookings SET status='cancel_pending',next_attempt=0 WHERE id=? AND status IN ('pending','confirmed')", b.ID)
	return e
}
