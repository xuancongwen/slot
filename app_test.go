package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type fakeCalendar struct {
	mu                            sync.Mutex
	busy                          []Span
	busyErr, insertErr, deleteErr error
	insertCalls, deleteCalls      int
	events                        map[string]bool
}

func (f *fakeCalendar) Busy(context.Context, []Calendar, time.Time, time.Time) ([]Span, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy, f.busyErr
}

const fakeMeetLink = "https://meet.google.com/abc-defg-hij"

func (f *fakeCalendar) Insert(_ context.Context, _ Calendar, b Booking) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertCalls++
	f.events[b.ID] = true
	if b.Meet && f.insertErr == nil {
		return fakeMeetLink, nil
	}
	return "", f.insertErr
}
func (f *fakeCalendar) Delete(_ context.Context, _ Calendar, b Booking) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.events, b.ID)
	return nil
}

func testApp(t *testing.T) (*App, *fakeCalendar) {
	t.Helper()
	a, e := newApp(Config{DataDir: t.TempDir(), PublicURL: "http://localhost:8080", AdminURL: "http://localhost:8081", RegistrationOpen: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.db.Close() })
	f := &fakeCalendar{events: map[string]bool{}}
	a.google = f
	return a, f
}
func seedHost(t *testing.T, a *App, slug string) User {
	t.Helper()
	r, e := a.db.Exec(`INSERT INTO users(email,password,name,slug,enabled,created) VALUES(?,?,'Alex',?,1,?)`, slug+"@example.com", "unused", slug, time.Now().Unix())
	if e != nil {
		t.Fatal(e)
	}
	uid, _ := r.LastInsertId()
	if _, e = a.db.Exec(`INSERT INTO meeting_types(user_id,slug,name,days,notice) VALUES(?,'chat','Chat','0123456',0)`, uid); e != nil {
		t.Fatal(e)
	}
	if _, e = a.db.Exec(`INSERT INTO locations(user_id,kind,label,detail) VALUES(?,'meet','Google Meet',''),(?,'custom','Zoom','https://zoom.us/j/1')`, uid, uid); e != nil {
		t.Fatal(e)
	}
	if _, e = a.db.Exec(`UPDATE users SET default_location=(SELECT min(id) FROM locations WHERE user_id=?) WHERE id=?`, uid, uid); e != nil {
		t.Fatal(e)
	}
	r, e = a.db.Exec("INSERT INTO accounts(user_id,identity,token) VALUES(?,?,?)", uid, slug+"@example.com", a.seal([]byte(`{"access_token":"test"}`)))
	if e != nil {
		t.Fatal(e)
	}
	aid, _ := r.LastInsertId()
	r, e = a.db.Exec("INSERT INTO calendars(account_id,google_id,name,role,check_busy) VALUES(?,?,'Work','owner',1)", aid, slug+"@example.com")
	if e != nil {
		t.Fatal(e)
	}
	cid, _ := r.LastInsertId()
	if _, e = a.db.Exec("UPDATE users SET write_calendar=? WHERE id=?", cid, uid); e != nil {
		t.Fatal(e)
	}
	u, e := a.userByID(context.Background(), uid)
	if e != nil {
		t.Fatal(e)
	}
	return u
}
func chatType(t *testing.T, a *App, u User) MeetingType {
	t.Helper()
	mt, e := scanMeetingType(a.db.QueryRow("SELECT "+meetingTypeColumns+" FROM meeting_types WHERE user_id=? AND slug='chat'", u.ID))
	if e != nil {
		t.Fatal(e)
	}
	return mt
}
func locationID(t *testing.T, a *App, u User, label string) string {
	t.Helper()
	var id int64
	if e := a.db.QueryRow("SELECT id FROM locations WHERE user_id=? AND label=?", u.ID, label).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return fmt.Sprint(id)
}
func mustLoad(t *testing.T, tz string) *time.Location {
	t.Helper()
	loc, e := time.LoadLocation(tz)
	if e != nil {
		t.Fatal(e)
	}
	return loc
}
func bookingFor(u User, start time.Time) Booking {
	return Booking{ID: randomHex(16), UserID: u.ID, CalendarID: u.WriteCalendar.Int64, GuestName: "Guest", GuestEmail: "guest@example.com", Start: start.Unix(), End: start.Add(30 * time.Minute).Unix(), BlockStart: start.Unix(), BlockEnd: start.Add(30 * time.Minute).Unix(), Title: "Guest / Alex", Timezone: "UTC", ManageToken: randomHex(32), Created: time.Now().Unix()}
}
func tomorrow() time.Time {
	n := time.Now().UTC().AddDate(0, 0, 1)
	return time.Date(n.Year(), n.Month(), n.Day(), 9, 0, 0, 0, time.UTC)
}
func formRequest(h http.Handler, path string, form url.Values, admin bool, extra ...*http.Cookie) *httptest.ResponseRecorder {
	form.Set("csrf", strings.Repeat("a", 64))
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	name := "slot_public_csrf"
	if admin {
		name = "slot_admin_csrf"
	}
	r.AddCookie(&http.Cookie{Name: name, Value: strings.Repeat("a", 64)})
	for _, c := range extra {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func sessionFor(t *testing.T, a *App, u User) *http.Cookie {
	t.Helper()
	s := randomHex(32)
	_, e := a.db.Exec("INSERT INTO sessions VALUES(?,?,?)", hashToken(s), u.ID, time.Now().Add(time.Hour).Unix())
	if e != nil {
		t.Fatal(e)
	}
	return &http.Cookie{Name: "slot_session", Value: s}
}

func TestAvailabilityRules(t *testing.T) {
	u := MeetingType{Timezone: "UTC", Days: "12345", StartMin: 540, EndMin: 660, Duration: 30, Buffer: 10, Notice: 60, Horizon: 30}
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	now := day.Add(8 * time.Hour)
	busy := []Span{{day.Add(9*time.Hour + 30*time.Minute), day.Add(10 * time.Hour)}}
	s := generateSlots(u, day, now, busy)
	if len(s) != 1 || s[0].Start != day.Add(10*time.Hour+30*time.Minute).Unix() {
		t.Fatalf("buffer/notice: %+v", s)
	}
	if got := generateSlots(u, day.AddDate(0, 0, 5), now, nil); len(got) != 0 {
		t.Fatal("Saturday should be closed")
	}
	if got := generateSlots(u, day.AddDate(0, 0, 40), now, nil); len(got) != 0 {
		t.Fatal("outside horizon")
	}
}
func TestDST(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	u := MeetingType{Timezone: loc.String(), Days: "0", StartMin: 120, EndMin: 180, Duration: 30, Horizon: 30}
	spring := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	if s := generateSlots(u, spring, spring.AddDate(0, 0, -1), nil); len(s) != 0 {
		t.Fatalf("spring gap exposed nonexistent times: %+v", s)
	}
	fall := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	u.StartMin = 60
	u.EndMin = 180
	s := generateSlots(u, fall, fall.AddDate(0, 0, -1), nil)
	if len(s) != 6 {
		t.Fatalf("fall repeated hour: got %d: %+v", len(s), s)
	}
	seen := map[int64]bool{}
	for _, x := range s {
		if seen[x.Start] {
			t.Fatal("duplicate instant")
		}
		seen[x.Start] = true
	}
	if s[0].Label == s[2].Label {
		t.Fatal("ambiguous repeated hour labels")
	}
}
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
func TestPublicBookingFlow(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	h := a.publicHandler()
	day := tomorrow()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/b/alex/chat?tz=UTC&date="+day.Format("2006-01-02"), nil))
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
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", manage, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Download calendar event") || !strings.Contains(w.Body.String(), fakeMeetLink) {
		t.Fatalf("manage: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "Guest <script>") {
		t.Fatal("unescaped guest name")
	}
	if want := day.In(mustLoad(t, "Asia/Singapore")).Format("15:04"); !strings.Contains(w.Body.String(), want) {
		t.Fatalf("manage page not in guest timezone, want %s", want)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", manage+"/event.ics", nil))
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
func TestFailClosedAndDaysOff(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	day := tomorrow()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	f.busyErr = errors.New("Google unavailable")
	if _, e := a.availability(context.Background(), u, mt, day, day.AddDate(0, 0, 1), time.Now()); e == nil {
		t.Fatal("availability allowed on Google error")
	}
	f.busyErr = nil
	_, e := a.db.Exec("INSERT INTO blocks(user_id,day) VALUES(?,?)", u.ID, day.Format("2006-01-02"))
	if e != nil {
		t.Fatal(e)
	}
	s, e := a.availability(context.Background(), u, mt, day, day.AddDate(0, 0, 1), time.Now())
	if e != nil || len(s) > 0 {
		t.Fatal("day off ignored")
	}
}

// A guest's day in Singapore spans two of a UTC meeting type's days; only slots
// starting inside the guest's day belong to it.
func TestAvailabilitySpansGuestDay(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	a.db.Exec("UPDATE meeting_types SET start_min=540,end_min=660 WHERE user_id=?", u.ID)
	mt := chatType(t, a, u)
	sgt := mustLoad(t, "Asia/Singapore")
	d := time.Now().In(sgt).AddDate(0, 0, 3)
	guestDay := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, sgt)
	slots, e := a.availability(context.Background(), u, mt, guestDay, guestDay.AddDate(0, 0, 1), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	var got []string
	for _, s := range slots {
		got = append(got, time.Unix(s.Start, 0).UTC().Format("Jan 2 15:04"))
	}
	hostDay := guestDay.Format("Jan 2") // 00:00–24:00 SGT covers 09:00–11:00 UTC of the same date.
	if want := []string{hostDay + " 09:00", hostDay + " 09:30", hostDay + " 10:00", hostDay + " 10:30"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("slots %v, want %v", got, want)
	}
	a.db.Exec("INSERT INTO blocks(user_id,day) VALUES(?,?)", u.ID, guestDay.Format("2006-01-02"))
	if slots, _ = a.availability(context.Background(), u, mt, guestDay, guestDay.AddDate(0, 0, 1), time.Now()); len(slots) != 0 {
		t.Fatal("day off in the meeting type's timezone ignored")
	}
}
func TestGuestCalendarPage(t *testing.T) {
	a, _ := testApp(t)
	seedHost(t, a, "alex")
	h := a.publicHandler()
	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
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
func TestPortsCSRFAndUserIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	b := bookingFor(v, tomorrow())
	a.reserve(context.Background(), b)
	for _, path := range []string{"/login", "/register", "/oauth/callback", "/settings", "/types/new"} {
		w := httptest.NewRecorder()
		a.publicHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("admin path %s exposed: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/logout", nil)
	r.AddCookie(cookie)
	a.adminHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w = formRequest(a.adminHandler(), "/bookings/"+b.ID+"/cancel", url.Values{}, true, cookie)
	if w.Code != 404 {
		t.Fatal("cross-user cancellation permitted")
	}
	w = formRequest(a.adminHandler(), "/calendars", url.Values{"write": {fmt.Sprint(v.WriteCalendar.Int64)}}, true, cookie)
	if w.Code != 400 {
		t.Fatal("cross-user calendar write permitted")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	a.adminHandler().ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "two@example.com") {
		t.Fatalf("dashboard isolation: %d", w.Code)
	}
}

// Browsers send "Origin: null" on form POSTs from a no-referrer page, which the
// origin check rejects, so every form would fail in a real browser.
func TestReferrerPolicyKeepsSameOriginPostsValid(t *testing.T) {
	a, _ := testApp(t)
	for _, tc := range []struct {
		name string
		h    http.Handler
		path string
	}{
		{"public", a.publicHandler(), "/"},
		{"admin", a.adminHandler(), "/login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
				t.Fatalf("Referrer-Policy = %q, want same-origin", got)
			}
		})
	}
}
func TestRegisterLoginPassword(t *testing.T) {
	a, _ := testApp(t)
	h := a.adminHandler()
	form := url.Values{"name": {"New Host"}, "slug": {"new-host"}, "email": {"new@example.com"}, "password": {"a long test password"}, "timezone": {"Asia/Singapore"}}
	w := formRequest(h, "/register", form, true)
	if w.Code != 303 {
		t.Fatalf("registration: %d %s", w.Code, w.Body)
	}
	var starterZone string
	a.db.QueryRow("SELECT m.timezone FROM meeting_types m JOIN users u ON u.id=m.user_id WHERE u.email='new@example.com'").Scan(&starterZone)
	if starterZone != "Asia/Singapore" {
		t.Fatalf("starter meeting type timezone = %q", starterZone)
	}
	var defaultKind string
	a.db.QueryRow("SELECT l.kind FROM users u JOIN locations l ON l.id=u.default_location WHERE u.email='new@example.com'").Scan(&defaultKind)
	if defaultKind != "meet" {
		t.Fatalf("default location kind = %q, want meet", defaultKind)
	}
	var c *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "slot_session" {
			c = cookie
		}
	}
	if c == nil {
		t.Fatal("session not issued")
	}
	w = formRequest(h, "/logout", url.Values{}, true, c)
	if w.Code != 303 {
		t.Fatal("logout failed")
	}
	w = formRequest(h, "/login", url.Values{"email": {"new@example.com"}, "password": {"wrong"}}, true)
	if w.Code != 401 {
		t.Fatal("bad password accepted")
	}
	w = formRequest(h, "/login", url.Values{"email": {"new@example.com"}, "password": {"a long test password"}}, true)
	if w.Code != 303 {
		t.Fatal("login failed")
	}
	var hash string
	a.db.QueryRow("SELECT password FROM users WHERE email='new@example.com'").Scan(&hash)
	if strings.Contains(hash, "test password") || !passwordMatches(hash, "a long test password") {
		t.Fatal("password storage")
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

func TestGoogleBusyErrorsAndBatches(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	var calls int
	fail := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing bearer")
		}
		if r.URL.Path != "/freeBusy" {
			t.Error(r.URL.Path)
		}
		var body struct{ Items []struct{ ID string } }
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Items) > 50 {
			t.Error("exceeded Google batch size")
		}
		calls++
		result := map[string]any{}
		for _, item := range body.Items {
			if fail {
				result[item.ID] = map[string]any{"errors": []any{map[string]string{"reason": "notFound"}}}
			} else {
				result[item.ID] = map[string]any{"busy": []any{map[string]string{"start": "2026-10-05T09:00:00Z", "end": "2026-10-05T10:00:00Z"}}}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"calendars": result})
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	cs := make([]Calendar, 51)
	for i := range cs {
		cs[i] = c
		cs[i].GoogleID = fmt.Sprint(i)
	}
	busy, e := g.Busy(context.Background(), cs, tomorrow(), tomorrow().Add(time.Hour))
	if e != nil || calls != 2 || len(busy) != 51 {
		t.Fatalf("batching: %d %d %v", calls, len(busy), e)
	}
	fail = true
	if _, e = g.Busy(context.Background(), cs[:1], tomorrow(), tomorrow().Add(time.Hour)); e == nil {
		t.Fatal("partial Google error ignored")
	}
}
func TestGoogleInsertConflictAndDelete(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	b := bookingFor(u, tomorrow())
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			posts++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != b.ID || r.URL.Query().Get("sendUpdates") != "all" {
				t.Error("event ID/invitation missing")
			}
			w.WriteHeader(409)
		case "GET":
			json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "extendedProperties": map[string]any{"private": map[string]string{"slotBooking": b.ID}}})
		case "DELETE":
			w.WriteHeader(410)
		}
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	if _, e := g.Insert(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if e := g.Delete(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if posts != 1 {
		t.Fatal(posts)
	}
}
func TestGoogleInsertRequestsMeet(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	b := bookingFor(u, tomorrow())
	b.Meet = true
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConferenceData struct {
				CreateRequest struct {
					RequestID             string
					ConferenceSolutionKey struct{ Type string }
				}
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		req := body.ConferenceData.CreateRequest
		if r.URL.Query().Get("conferenceDataVersion") != "1" || req.RequestID != b.ID || req.ConferenceSolutionKey.Type != "hangoutsMeet" {
			t.Errorf("Meet not requested: %s %+v", r.URL.RawQuery, req)
		}
		json.NewEncoder(w).Encode(map[string]string{"hangoutLink": fakeMeetLink})
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	link, e := g.Insert(context.Background(), c, b)
	if e != nil || link != fakeMeetLink {
		t.Fatalf("link %q, error %v", link, e)
	}
}
func TestGuestLocationChoice(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	theirs := seedHost(t, a, "two")
	for _, tc := range []struct {
		name, choice, custom, want string
		meet, fails                bool
	}{
		{name: "Google Meet", choice: locationID(t, a, u, "Google Meet"), meet: true},
		{name: "host link", choice: locationID(t, a, u, "Zoom"), want: "https://zoom.us/j/1"},
		{name: "guest's own text wins", choice: locationID(t, a, u, "Zoom"), custom: " Call me at 555-0100 ", want: "Call me at 555-0100"},
		{name: "nothing chosen"},
		{name: "another host's location", choice: locationID(t, a, theirs, "Zoom"), fails: true},
		{name: "too long", custom: strings.Repeat("x", 501), fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.PostForm = url.Values{"location": {tc.choice}, "custom_location": {tc.custom}}
			text, meet, e := a.chosenLocation(r, u)
			if (e != nil) != tc.fails || text != tc.want || meet != tc.meet {
				t.Fatalf("got %q meet=%v err=%v", text, meet, e)
			}
		})
	}
}
func TestLocationEditingIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	theirs := locationID(t, a, v, "Zoom")
	formRequest(h, "/locations/"+theirs+"/delete", url.Values{}, true, cookie)
	formRequest(h, "/locations/"+theirs+"/default", url.Values{}, true, cookie)
	var n int
	a.db.QueryRow("SELECT count(*) FROM locations WHERE id=?", theirs).Scan(&n)
	if n != 1 {
		t.Fatal("deleted another host's location")
	}
	if got, _ := a.userByID(context.Background(), u.ID); got.DefaultLocation != u.DefaultLocation {
		t.Fatalf("default changed to %v", got.DefaultLocation)
	}
	if w := formRequest(h, "/locations", url.Values{"label": {"Office"}, "detail": {"1 Main St"}}, true, cookie); w.Code != 303 {
		t.Fatalf("add: %d", w.Code)
	}
	office := locationID(t, a, u, "Office")
	formRequest(h, "/locations/"+office+"/default", url.Values{}, true, cookie)
	if got, _ := a.userByID(context.Background(), u.ID); fmt.Sprint(got.DefaultLocation.Int64) != office {
		t.Fatal("default not changed")
	}
}
func TestReorderLocations(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	order := func(u User) string {
		ls, e := a.locations(context.Background(), u)
		if e != nil {
			t.Fatal(e)
		}
		var labels []string
		for _, l := range ls {
			labels = append(labels, l.Label)
		}
		return fmt.Sprint(labels)
	}
	formRequest(h, "/locations", url.Values{"label": {"Office"}}, true, cookie)
	for _, tc := range []struct{ label, direction, want string }{
		{"Office", "up", "[Google Meet Office Zoom]"},
		{"Office", "up", "[Office Google Meet Zoom]"},
		{"Office", "up", "[Office Google Meet Zoom]"},
		{"Google Meet", "down", "[Office Zoom Google Meet]"},
	} {
		formRequest(h, "/locations/"+locationID(t, a, u, tc.label)+"/move", url.Values{"direction": {tc.direction}}, true, cookie)
		if got := order(u); got != tc.want {
			t.Fatalf("after moving %s %s: %s, want %s", tc.label, tc.direction, got, tc.want)
		}
	}
	formRequest(h, "/locations/"+locationID(t, a, v, "Zoom")+"/move", url.Values{"direction": {"up"}}, true, cookie)
	if got := order(v); got != "[Google Meet Zoom]" {
		t.Fatalf("another host's locations reordered: %s", got)
	}
}
func TestTokenRefreshEncryptedAndPersistent(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	old := oauth2.Token{AccessToken: "expired", RefreshToken: "refresh-secret", Expiry: time.Now().Add(-time.Hour)}
	data, _ := json.Marshal(old)
	a.db.Exec("UPDATE accounts SET token=? WHERE id=?", a.seal(data), c.AccountID)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-secret" {
			t.Error("missing refresh token")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh-secret","token_type":"Bearer","expires_in":3600}`)
	}))
	defer s.Close()
	a.oauth.Endpoint.TokenURL = s.URL
	g := &Google{app: a}
	token, e := g.token(context.Background(), c.AccountID)
	if e != nil || token != "fresh-secret" {
		t.Fatalf("refresh: %s %v", token, e)
	}
	var encrypted []byte
	a.db.QueryRow("SELECT token FROM accounts WHERE id=?", c.AccountID).Scan(&encrypted)
	if strings.Contains(string(encrypted), "secret") {
		t.Fatal("plaintext secret")
	}
	data, e = a.open(encrypted)
	if e != nil || !strings.Contains(string(data), "refresh-secret") {
		t.Fatal("refresh token not preserved")
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
	second, e := newApp(a.cfg)
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
func TestMissingKeyDoesNotReinitialize(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "slot.db"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := newApp(Config{DataDir: dir}); e == nil {
		t.Fatal("silently recreated encryption key")
	}
}
func TestSchemaForeignKeys(t *testing.T) {
	a, _ := testApp(t)
	var enabled int
	if e := a.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); e != nil || enabled != 1 {
		t.Fatal("foreign keys disabled")
	}
}

func TestTwoCharacterBookingSlug(t *testing.T) {
	for _, slug := range []string{"a", "al", "alex", "alex-morgan"} {
		if !slugPattern.MatchString(slug) {
			t.Errorf("valid slug rejected: %s", slug)
		}
	}
	for _, slug := range []string{"", "-al", "al-", "../al", "al/ex", strings.Repeat("a", 41)} {
		if slugPattern.MatchString(slug) {
			t.Errorf("invalid slug accepted: %s", slug)
		}
	}
}
func TestMeetingTypeEditingAndIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	theirs := chatType(t, a, seedHost(t, a, "two"))
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	form := func(slug, tz string) url.Values {
		return url.Values{"name": {"Deep dive"}, "slug": {slug}, "timezone": {tz}, "duration": {"60"}, "days": {"1", "2"}, "start": {"09:00"}, "end": {"17:00"}, "buffer": {"0"}, "notice": {"0"}, "horizon": {"30"}, "active": {"on"}}
	}
	for _, tc := range []struct {
		name, path string
		form       url.Values
		want       int
	}{
		{"create", "/types", form("deep", "Asia/Singapore"), 303},
		{"unknown timezone", "/types", form("mars", "Mars/Olympus_Mons"), 400},
		{"server-local timezone", "/types", form("local", "Local"), 400},
		{"duplicate URL", "/types", form("chat", "UTC"), 409},
		{"another host's type", fmt.Sprintf("/types/%d", theirs.ID), form("stolen", "UTC"), 404},
		{"delete another host's type", fmt.Sprintf("/types/%d/delete", theirs.ID), url.Values{}, 303},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := formRequest(h, tc.path, tc.form, true, cookie); w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
	var zone string
	var days string
	a.db.QueryRow("SELECT timezone,days FROM meeting_types WHERE user_id=? AND slug='deep'", u.ID).Scan(&zone, &days)
	if zone != "Asia/Singapore" || days != "12" {
		t.Fatalf("saved %q %q", zone, days)
	}
	var n int
	a.db.QueryRow("SELECT count(*) FROM meeting_types WHERE id=? AND slug='chat'", theirs.ID).Scan(&n)
	if n != 1 {
		t.Fatal("another host's meeting type was changed or deleted")
	}
}
func TestSlugify(t *testing.T) {
	for name, want := range map[string]string{
		"30 minute chat":           "30-minute-chat",
		"  Coffee chat (30 min)! ": "coffee-chat-30-min",
		"Café ☕":                   "caf",
		"☕☕":                       "meeting",
		strings.Repeat("ab ", 30):  "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-a", // Cut at the 40-character limit.
	} {
		if got := slugify(name); got != want || !slugPattern.MatchString(got) {
			t.Errorf("slugify(%q) = %q, want %q", name, got, want)
		}
	}
}
func TestBlankSlugComesFromName(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	cookie := sessionFor(t, a, u)
	form := url.Values{"name": {"Chat"}, "slug": {""}, "timezone": {"UTC"}, "duration": {"30"}, "days": {"1"}, "start": {"09:00"}, "end": {"17:00"}, "buffer": {"0"}, "notice": {"0"}, "horizon": {"30"}}
	for range 2 {
		if w := formRequest(a.adminHandler(), "/types", form, true, cookie); w.Code != 303 {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
	}
	rows, _ := a.db.Query("SELECT slug FROM meeting_types WHERE user_id=? ORDER BY id", u.ID)
	var slugs []string
	for rows.Next() {
		var slug string
		rows.Scan(&slug)
		slugs = append(slugs, slug)
	}
	rows.Close()
	if fmt.Sprint(slugs) != "[chat chat-2 chat-3]" {
		t.Fatalf("slugs %v", slugs)
	}
}
func TestHostPageListsActiveTypes(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	h := a.publicHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/b/alex"); w.Code != 302 || w.Header().Get("Location") != "/b/alex/chat" {
		t.Fatalf("single type should redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	a.db.Exec("INSERT INTO meeting_types(user_id,slug,name) VALUES(?,'deep','Deep dive')", u.ID)
	a.db.Exec("INSERT INTO meeting_types(user_id,slug,name,active) VALUES(?,'hidden','Secret',0)", u.ID)
	w := get("/b/alex")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Deep dive") || !strings.Contains(w.Body.String(), "Chat") || strings.Contains(w.Body.String(), "Secret") {
		t.Fatalf("type list: %d %s", w.Code, w.Body)
	}
	if w := get("/b/alex/hidden"); w.Code != 404 {
		t.Fatalf("inactive type reachable: %d", w.Code)
	}
}
func TestTimezoneListLoads(t *testing.T) {
	a, _ := testApp(t)
	if len(a.timezones) < 300 || a.timezones[0] != "UTC" {
		t.Fatalf("timezone list: %d entries", len(a.timezones))
	}
	for _, tz := range a.timezones {
		if !validTimezone(tz) {
			t.Errorf("listed timezone does not load: %s", tz)
		}
	}
}
func TestUpgradeFromVersion4(t *testing.T) {
	a, _ := testApp(t)
	seedHost(t, a, "alex")
	for _, q := range []string{"ALTER TABLE locations DROP COLUMN position", "PRAGMA user_version=4"} {
		if _, e := a.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	a.db.Close()
	b, e := newApp(a.cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer b.db.Close()
	var version, n int
	b.db.QueryRow("PRAGMA user_version").Scan(&version)
	if e = b.db.QueryRow("SELECT count(*) FROM locations WHERE position=0").Scan(&n); e != nil || version != schemaVersion || n != 2 {
		t.Fatalf("version %d, rows %d, error %v", version, n, e)
	}
}
func TestOldSchemaRefused(t *testing.T) {
	a, _ := testApp(t)
	if _, e := a.db.Exec("PRAGMA user_version=1"); e != nil {
		t.Fatal(e)
	}
	a.db.Close()
	if _, e := newApp(a.cfg); e == nil || !strings.Contains(e.Error(), "schema version") {
		t.Fatalf("old schema accepted: %v", e)
	}
}
func BenchmarkSlotGeneration(b *testing.B) {
	u := MeetingType{Timezone: "America/Los_Angeles", Days: "0123456", StartMin: 540, EndMin: 1020, Duration: 30, Horizon: 30}
	loc, _ := time.LoadLocation(u.Timezone)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	b.ReportAllocs()
	for b.Loop() {
		generateSlots(u, day, day.AddDate(0, 0, -1), nil)
	}
}
