package slot

import (
	"context"
	"errors"
	"testing"
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
