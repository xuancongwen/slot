package slot

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bookChat books alex's chat type as a guest and returns the booking.
func bookChat(t *testing.T, a *App, u User, start time.Time) Booking {
	t.Helper()
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), start.Unix())}, "name": {"Guest"}, "email": {"guest@example.com"}, "tz": {"UTC"}}
	w := formRequest(a.publicHandler(), "/b/"+u.Slug+"/chat", form, false)
	if w.Code != 303 {
		t.Fatalf("book: %d %s", w.Code, w.Body)
	}
	b, e := a.getBooking(context.Background(), strings.TrimPrefix(w.Header().Get("Location"), "/manage/"))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestGuestReschedulesBooking(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	if _, e := a.db.Exec("UPDATE meeting_types SET buffer=15 WHERE user_id=?", u.ID); e != nil {
		t.Fatal(e)
	}
	day := tomorrow()
	b := bookChat(t, a, u, day)
	a.reconcile(context.Background())
	h := a.publicHandler()
	manage := "/manage/" + b.ManageToken
	if w := getRequest(h, manage); !strings.Contains(w.Body.String(), manage+"/reschedule") {
		t.Fatalf("manage page offers no reschedule: %s", w.Body)
	}
	// Google reports the booking's own event, and its buffer overlaps the next slot.
	// Neither may keep the guest from moving into that slot.
	f.busy = []Span{{day, day.Add(30 * time.Minute)}}
	later := day.Add(30 * time.Minute)
	a.cfg.AnalyticsScript = "https://stats.example.com/script.js"
	w := getRequest(h, manage+"/reschedule?tz=UTC&date="+day.Format(dateLayout))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "start="+strconv.FormatInt(later.Unix(), 10)) {
		t.Fatalf("reschedule page: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "stats.example.com") || strings.Contains(w.Body.String(), "name=\"email\"") {
		t.Fatal("reschedule page asks for details or loads the tracker")
	}
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), later.Unix())}}
	for range 2 {
		if w = formRequest(h, manage+"/reschedule", form, false); w.Code != 303 || w.Header().Get("Location") != manage {
			t.Fatalf("reschedule: %d %s", w.Code, w.Body)
		}
	}
	got, _ := a.getBooking(context.Background(), b.ManageToken)
	want := Booking{Start: later.Unix(), End: later.Add(30 * time.Minute).Unix(), BlockStart: later.Add(-15 * time.Minute).Unix(), BlockEnd: later.Add(45 * time.Minute).Unix()}
	if got.Status != "pending" || got.Start != want.Start || got.End != want.End || got.BlockStart != want.BlockStart || got.BlockEnd != want.BlockEnd {
		t.Fatalf("moved booking %+v", got)
	}
	a.reconcile(context.Background())
	got, _ = a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "confirmed" || f.insertCalls != 2 || f.inserted.ID != b.ID || f.inserted.Start != later.Unix() {
		t.Fatalf("event not moved: status %s, %d inserts, %+v", got.Status, f.insertCalls, f.inserted)
	}
	if e := a.reserve(context.Background(), bookingFor(u, day.Add(-30*time.Minute))); e != nil {
		t.Fatal("old time not freed", e)
	}
}

func TestRescheduleRefusesTakenTime(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	day := tomorrow()
	b := bookChat(t, a, u, day)
	later := day.Add(time.Hour)
	other := bookingFor(u, later)
	if e := a.reserve(context.Background(), other); e != nil {
		t.Fatal(e)
	}
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), later.Unix())}}
	if w := formRequest(a.publicHandler(), "/manage/"+b.ManageToken+"/reschedule", form, false); w.Code != 409 {
		t.Fatalf("moved onto a booked time: %d", w.Code)
	}
	// A booking made after the guest's last availability check still wins.
	if e := a.moveBooking(context.Background(), b, chatType(t, a, u), later.Unix()); !errors.Is(e, errNotMoved) {
		t.Fatalf("racing move: %v", e)
	}
	got, _ := a.getBooking(context.Background(), b.ManageToken)
	if got.Start != day.Unix() {
		t.Fatal("refused move changed the booking")
	}
}

func TestRescheduleRequest(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	b := requestChat(t, a, u, tomorrow())
	later := tomorrow().Add(time.Hour)
	if w := getRequest(a.publicHandler(), "/manage/"+b.ManageToken); !strings.Contains(w.Body.String(), "Request a different time") {
		t.Fatalf("manage page: %s", w.Body)
	}
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), later.Unix())}}
	if w := formRequest(a.publicHandler(), "/manage/"+b.ManageToken+"/reschedule", form, false); w.Code != 303 {
		t.Fatalf("reschedule: %d %s", w.Code, w.Body)
	}
	a.reconcile(context.Background())
	got, _ := a.getBooking(context.Background(), b.ManageToken)
	if got.Status != "requested" || got.Start != later.Unix() || f.insertCalls != 0 {
		t.Fatalf("status %s, start %d, %d inserts", got.Status, got.Start, f.insertCalls)
	}
}

func TestRescheduleLimits(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *App, User, Booking)
	}{
		{"approved booking needing approval", func(t *testing.T, a *App, u User, b Booking) {
			mustExec(t, a, "UPDATE meeting_types SET approval=1 WHERE user_id=?", u.ID)
		}},
		{"cancelled", func(t *testing.T, a *App, u User, b Booking) {
			mustExec(t, a, "UPDATE bookings SET status='cancelled' WHERE id=?", b.ID)
		}},
		{"past", func(t *testing.T, a *App, u User, b Booking) {
			now := time.Now()
			mustExec(t, a, "UPDATE bookings SET start=?,end=?,block_start=?,block_end=? WHERE id=?", now.Add(-time.Hour).Unix(), now.Unix(), now.Add(-time.Hour).Unix(), now.Unix(), b.ID)
		}},
		{"paused type", func(t *testing.T, a *App, u User, b Booking) {
			mustExec(t, a, "UPDATE meeting_types SET active=0 WHERE user_id=?", u.ID)
		}},
		{"from before types were recorded", func(t *testing.T, a *App, u User, b Booking) {
			mustExec(t, a, "UPDATE bookings SET meeting_type_id=NULL WHERE id=?", b.ID)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testApp(t)
			u := seedHost(t, a, "alex")
			b := bookChat(t, a, u, tomorrow())
			a.reconcile(context.Background())
			tt.setup(t, a, u, b)
			h := a.publicHandler()
			manage := "/manage/" + b.ManageToken
			if w := getRequest(h, manage); strings.Contains(w.Body.String(), manage+"/reschedule") {
				t.Fatal("manage page offers reschedule")
			}
			if w := getRequest(h, manage+"/reschedule"); w.Code != 409 {
				t.Fatalf("reschedule page: %d", w.Code)
			}
			form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), tomorrow().Add(time.Hour).Unix())}}
			if w := formRequest(h, manage+"/reschedule", form, false); w.Code != 409 {
				t.Fatalf("reschedule: %d", w.Code)
			}
		})
	}
}

func mustExec(t *testing.T, a *App, query string, args ...any) {
	t.Helper()
	if _, e := a.db.Exec(query, args...); e != nil {
		t.Fatal(e)
	}
}
