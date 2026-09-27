package slot

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAtomicReservation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := bookingFor(u, tomorrow())
			if e := a.reserve(context.Background(), b); e == nil {
				successes.Add(1)
			} else if !strings.Contains(e.Error(), "slot_overlap") {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("accepted %d concurrent bookings", successes.Load())
	}
}

func TestSharedDestinationCannotDoubleBook(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	_, e := a.db.Exec("UPDATE calendars SET google_id='shared@example.com'")
	if e != nil {
		t.Fatal(e)
	}
	if e = a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil {
		t.Fatal(e)
	}
	if e = a.reserve(context.Background(), bookingFor(v, tomorrow())); e == nil {
		t.Fatal("shared calendar accepted overlap")
	}
}

func TestPublicBookingFlow(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	h := a.publicHandler()
	day := tomorrow()
	w := getRequest(h, "/b/alex/chat?tz=UTC&date="+day.Format("2006-01-02"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Choose a day") {
		t.Fatalf("page: %d %s", w.Code, w.Body)
	}
	ticket := a.ticket(u.ID, mt, day.Unix())
	form := url.Values{"ticket": {ticket}, "name": {"Guest <script>alert(1)</script>"}, "email": {"guest@example.com"}, "location": {locationID(t, a, u, "Google Meet")}, "tz": {"Asia/Singapore"}}
	w = formRequest(h, "/b/alex/chat", form, false)
	if w.Code != 303 {
		t.Fatalf("book: %d %s", w.Code, w.Body)
	}
	manage := w.Header().Get("Location")
	w2 := formRequest(h, "/b/alex/chat", form, false)
	if w2.Code != 303 || w2.Header().Get("Location") != manage {
		t.Fatal("duplicate submit did not return same booking")
	}
	a.reconcile(context.Background())
	var n int
	a.db.QueryRow("SELECT count(*) FROM bookings").Scan(&n)
	if n != 1 || f.insertCalls != 1 {
		t.Fatal("duplicate booking")
	}
	w = getRequest(h, manage)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Download calendar event") || !strings.Contains(w.Body.String(), fakeMeetLink) {
		t.Fatalf("manage: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "Guest <script>") {
		t.Fatal("unescaped guest name")
	}
	if want := day.In(mustLoad(t, "Asia/Singapore")).Format("15:04"); !strings.Contains(w.Body.String(), want) {
		t.Fatalf("manage page not in guest timezone, want %s", want)
	}
	w = getRequest(h, manage+"/event.ics")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "BEGIN:VEVENT") {
		t.Fatal("ICS missing")
	}
	w = formRequest(h, manage+"/cancel", url.Values{}, false)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	a.reconcile(context.Background())
	if f.deleteCalls != 1 {
		t.Fatal("not cancelled upstream")
	}
}

func TestHostPageListsActiveTypes(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	h := a.publicHandler()
	// A lone type's calendar shows in place; picking a date stays on /b/alex.
	if w := getRequest(h, "/b/alex"); w.Code != 200 || !strings.Contains(w.Body.String(), `href="/b/alex?tz=`) || !strings.Contains(w.Body.String(), "Choose a day") {
		t.Fatalf("single type: %d %s", w.Code, w.Body)
	}
	a.db.Exec("INSERT INTO meeting_types(user_id,slug,name) VALUES(?,'deep','Deep dive')", u.ID)
	a.db.Exec("INSERT INTO meeting_types(user_id,slug,name,active) VALUES(?,'hidden','Secret',0)", u.ID)
	w := getRequest(h, "/b/alex")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Deep dive") || !strings.Contains(w.Body.String(), "Chat") || strings.Contains(w.Body.String(), "Secret") {
		t.Fatalf("type list: %d %s", w.Code, w.Body)
	}
	if w := getRequest(h, "/b/alex/hidden"); w.Code != 404 {
		t.Fatalf("inactive type reachable: %d", w.Code)
	}
}

func TestSingleHostRoot(t *testing.T) {
	a, _ := testApp(t)
	a.cfg.SingleHost = "alex"
	h := a.publicHandler()
	if body := getRequest(h, "/").Body.String(); !strings.Contains(body, "Open the booking link") {
		t.Fatal("root before the host registers should be the landing page")
	}
	u := seedHost(t, a, "alex")
	seedHost(t, a, "other") // Only SINGLE_HOST is served at the root.
	// The lone type's calendar is the root: month, date, and time links and the
	// timezone form all stay on /, while booking still posts to the type's URL.
	day := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")
	w := getRequest(h, "/?tz=UTC&date="+day)
	body := w.Body.String()
	for _, want := range []string{`href="/?tz=UTC&amp;month=`, `action="/"`, "&amp;start="} {
		if !strings.Contains(body, want) {
			t.Errorf("root calendar missing %q", want)
		}
	}
	if w.Code != 200 {
		t.Fatalf("root: %d", w.Code)
	}
	var start string
	if i := strings.Index(body, "&amp;start="); i >= 0 {
		start = body[i+len("&amp;start=") : i+len("&amp;start=")+10]
	}
	if form := getRequest(h, "/?tz=UTC&date="+day+"&start="+start).Body.String(); !strings.Contains(form, `action="/b/alex/chat"`) {
		t.Fatal("booking form should post to the type's own URL")
	}
	a.db.Exec("INSERT INTO meeting_types(user_id,slug,name) VALUES(?,'deep','Deep dive')", u.ID)
	body = getRequest(h, "/").Body.String()
	if !strings.Contains(body, `href="/b/alex/deep"`) || !strings.Contains(body, `href="/b/alex/chat"`) {
		t.Fatalf("root should list types at their own URLs: %s", body)
	}
	a.cfg.SingleHost = ""
	if body = getRequest(h, "/").Body.String(); !strings.Contains(body, "Open the booking link") {
		t.Fatal("multi-host root should stay the landing page")
	}
}
func TestGuestCalendarPage(t *testing.T) {
	a, _ := testApp(t)
	seedHost(t, a, "alex")
	h := a.publicHandler()
	get := func(path string) string {
		t.Helper()
		w := getRequest(h, path)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	sgt := mustLoad(t, "Asia/Singapore")
	date := time.Now().In(sgt).AddDate(0, 0, 2).Format("2006-01-02")
	body := get("/b/alex/chat?tz=Asia/Singapore&date=" + date)
	for _, want := range []string{`aria-current="date"`, "date=" + date, "17:00", "Asia/Singapore"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "data-detect-guest-timezone") {
		t.Error("explicit timezone should not be re-detected")
	}
	if body = get("/b/alex/chat"); !strings.Contains(body, `data-detect-guest-timezone="UTC"`) {
		t.Error("page without tz should ask the browser for its zone")
	}
	if body = get("/b/alex/chat?tz=Not/AZone"); !strings.Contains(body, "UTC") {
		t.Error("invalid timezone did not fall back to the meeting type's")
	}
}

func TestCalendarWeeks(t *testing.T) {
	october := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) // A Thursday.
	weeks := calendarWeeks(october, "2026-10-05", map[string][]Slot{"2026-10-05": {{}}})
	if len(weeks) != 5 || weeks[0][4].Day != 1 || weeks[0][3].InMonth || !weeks[4][6].InMonth {
		t.Fatalf("layout: %+v", weeks)
	}
	if monday := weeks[1][1]; monday.Date != "2026-10-05" || !monday.Open || !monday.Selected {
		t.Fatalf("October 5: %+v", monday)
	}
}

func TestGuestLabelsMarkRepeatedHour(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, ny) // EDT; the same wall time repeats in EST.
	s := guestLabels([]Slot{{Start: first.Unix()}, {Start: first.Add(time.Hour).Unix()}}, ny)
	if s[0].Label != "01:30 (UTC-04:00)" || s[1].Label != "01:30 (UTC-05:00)" {
		t.Fatalf("labels %q %q", s[0].Label, s[1].Label)
	}
	if s = guestLabels([]Slot{{Start: first.AddDate(0, 0, 1).Unix()}}, ny); s[0].Label != "01:30" {
		t.Fatalf("ordinary day label %q", s[0].Label)
	}
}

func TestTicketIntegrity(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	ticket := a.ticket(u.ID, mt, tomorrow().Unix())
	if _, _, _, e := a.verifyTicket(ticket, u, mt); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e := a.verifyTicket(ticket+"x", u, mt); e == nil {
		t.Fatal("tampered ticket accepted")
	}
	other := mt
	other.ID++
	if _, _, _, e := a.verifyTicket(ticket, u, other); e == nil {
		t.Fatal("ticket accepted for another meeting type")
	}
	mt.Duration = 60
	if _, _, _, e := a.verifyTicket(ticket, u, mt); e == nil {
		t.Fatal("old duration accepted")
	}
}
