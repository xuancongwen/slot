package slot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDurableSyncAndCancellation(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	b := bookingFor(u, tomorrow())
	if e := a.reserve(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	f.insertErr = errors.New("timeout after insert")
	a.syncBooking(context.Background(), b.ID)
	got, e := a.getBooking(context.Background(), b.ManageToken)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != "pending" || got.Attempts != 1 {
		t.Fatalf("lost pending state: %+v", got)
	}
	if e = a.reserve(context.Background(), bookingFor(u, tomorrow())); e == nil {
		t.Fatal("uncertain booking did not block time")
	}
	f.insertErr = nil
	a.syncBooking(context.Background(), b.ID)
	got, _ = a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "confirmed" || len(f.events) != 1 {
		t.Fatal("retry not idempotent")
	}
	if e = a.cancelBooking(context.Background(), got); e != nil {
		t.Fatal(e)
	}
	f.deleteErr = errors.New("Google offline")
	a.syncBooking(context.Background(), b.ID)
	got, _ = a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "cancel_pending" {
		t.Fatal("slot released before cancellation")
	}
	f.deleteErr = nil
	a.syncBooking(context.Background(), b.ID)
	got, _ = a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "cancelled" || len(f.events) != 0 {
		t.Fatal("not cancelled")
	}
	if e = a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil {
		t.Fatal("cancelled slot not reusable", e)
	}
}

func TestRestartKeepsPendingBookings(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	b := bookingFor(u, tomorrow())
	if e := a.reserve(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	mt := chatType(t, a, u)
	a.db.Close()
	second, e := New(a.cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer second.db.Close()
	f := &fakeCalendar{events: map[string]bool{}}
	second.google = f
	second.reconcile(context.Background())
	got, e := second.getBooking(context.Background(), b.ManageToken)
	if e != nil || got.Status != "confirmed" {
		t.Fatal("restart lost booking", e)
	}
	ticket := a.ticket(u.ID, mt, tomorrow().Unix())
	if _, _, _, e = second.verifyTicket(ticket, u, mt); e != nil {
		t.Fatal("ticket key changed on restart")
	}
}

func TestCheckAppliesGoogleChanges(t *testing.T) {
	start := tomorrow()
	later := start.Add(2 * time.Hour)
	tests := []struct {
		name       string
		change     func(*fakeCalendar, Booking)
		wantStatus string
		wantStart  int64
		wantError  bool
	}{
		{"unchanged", func(*fakeCalendar, Booking) {}, "confirmed", start.Unix(), false},
		{"deleted in Google", func(f *fakeCalendar, b Booking) { delete(f.events, b.ID) }, "cancelled", start.Unix(), false},
		{"guest declined", func(f *fakeCalendar, _ Booking) { f.remote = EventState{Declined: true} }, "cancel_pending", start.Unix(), false},
		{"moved in Google", func(f *fakeCalendar, _ Booking) {
			f.remote = EventState{Start: later.Unix(), End: later.Add(30 * time.Minute).Unix()}
		}, "confirmed", later.Unix(), false},
		{"all-day in Google", func(f *fakeCalendar, _ Booking) { f.remote = EventState{} }, "confirmed", start.Unix(), false},
		{"Google unreachable", func(f *fakeCalendar, _ Booking) { f.checkErr = errors.New("Google offline") }, "confirmed", start.Unix(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, f := testApp(t)
			u := seedHost(t, a, "alex")
			b := bookingFor(u, start)
			b.BlockStart, b.BlockEnd = b.Start-600, b.End+600
			if e := a.reserve(context.Background(), b); e != nil {
				t.Fatal(e)
			}
			a.reconcile(context.Background())
			tt.change(f, b)
			dueForCheck(t, a)
			a.reconcile(context.Background())
			got, e := a.getBooking(context.Background(), b.ManageToken)
			if e != nil {
				t.Fatal(e)
			}
			if got.Status != tt.wantStatus || got.Start != tt.wantStart || got.End != tt.wantStart+1800 || got.BlockStart != tt.wantStart-600 || got.BlockEnd != tt.wantStart+2400 || (got.LastError != "") != tt.wantError {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDeclineRemovesEvent(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	b := bookingFor(u, tomorrow())
	if e := a.reserve(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	a.reconcile(context.Background())
	f.remote = EventState{Declined: true}
	dueForCheck(t, a)
	a.reconcile(context.Background())
	a.reconcile(context.Background())
	got, _ := a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "cancelled" || f.deleteCalls != 1 || len(f.events) != 0 {
		t.Fatalf("status %s, deletes %d", got.Status, f.deleteCalls)
	}
	if e := a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil {
		t.Fatal("declined slot not reusable", e)
	}
}

func TestCheckSkipsRecentAndPastBookings(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	past := bookingFor(u, time.Now().Add(-2*time.Hour))
	for _, b := range []Booking{bookingFor(u, tomorrow()), past} {
		if e := a.reserve(context.Background(), b); e != nil {
			t.Fatal(e)
		}
	}
	a.reconcile(context.Background())
	a.reconcile(context.Background())
	if f.checkCalls != 0 {
		t.Fatalf("%d checks of fresh bookings, want 0", f.checkCalls)
	}
	dueForCheck(t, a)
	a.reconcile(context.Background())
	a.reconcile(context.Background())
	if f.checkCalls != 1 {
		t.Fatalf("%d checks after the interval, want 1", f.checkCalls)
	}
}

// dueForCheck makes every booking's last check a full interval old.
func dueForCheck(t *testing.T, a *App) {
	t.Helper()
	if _, e := a.db.Exec("UPDATE bookings SET checked=?", time.Now().Add(-checkInterval).Unix()); e != nil {
		t.Fatal(e)
	}
}
