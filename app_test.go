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
func (f *fakeCalendar) Insert(_ context.Context, _ Calendar, b Booking) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertCalls++
	f.events[b.ID] = true
	return f.insertErr
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
	r, e := a.db.Exec(`INSERT INTO users(email,password,name,slug,days,notice,enabled,created) VALUES(?,?,'Alex',?,'0123456',0,1,?)`, slug+"@example.com", "unused", slug, time.Now().Unix())
	if e != nil {
		t.Fatal(e)
	}
	uid, _ := r.LastInsertId()
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
	u := User{Timezone: "UTC", Days: "12345", StartMin: 540, EndMin: 660, Duration: 30, Buffer: 10, Notice: 60, Horizon: 30}
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
	u := User{Timezone: loc.String(), Days: "0", StartMin: 120, EndMin: 180, Duration: 30, Horizon: 30}
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
	h := a.publicHandler()
	day := tomorrow()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/b/alex?date="+day.Format("2006-01-02"), nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Choose a day") {
		t.Fatalf("page: %d %s", w.Code, w.Body)
	}
	ticket := a.ticket(u.ID, day.Unix(), u.Duration)
	form := url.Values{"ticket": {ticket}, "name": {"Guest <script>alert(1)</script>"}, "email": {"guest@example.com"}}
	w = formRequest(h, "/b/alex", form, false)
	if w.Code != 303 {
		t.Fatalf("book: %d %s", w.Code, w.Body)
	}
	manage := w.Header().Get("Location")
	w2 := formRequest(h, "/b/alex", form, false)
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
	if w.Code != 200 || !strings.Contains(w.Body.String(), "You’re on") {
		t.Fatalf("manage: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "Guest <script>") {
		t.Fatal("unescaped guest name")
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
	day := tomorrow()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	f.busyErr = errors.New("Google unavailable")
	if _, e := a.availability(context.Background(), u, day, time.Now()); e == nil {
		t.Fatal("availability allowed on Google error")
	}
	f.busyErr = nil
	_, e := a.db.Exec("INSERT INTO blocks(user_id,day) VALUES(?,?)", u.ID, day.Format("2006-01-02"))
	if e != nil {
		t.Fatal(e)
	}
	s, e := a.availability(context.Background(), u, day, time.Now())
	if e != nil || len(s) > 0 {
		t.Fatal("day off ignored")
	}
}
func TestPortsCSRFAndUserIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	b := bookingFor(v, tomorrow())
	a.reserve(context.Background(), b)
	for _, path := range []string{"/login", "/register", "/oauth/callback", "/settings"} {
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
func TestRegisterLoginPassword(t *testing.T) {
	a, _ := testApp(t)
	h := a.adminHandler()
	form := url.Values{"name": {"New Host"}, "slug": {"new-host"}, "email": {"new@example.com"}, "password": {"a long test password"}}
	w := formRequest(h, "/register", form, true)
	if w.Code != 303 {
		t.Fatalf("registration: %d %s", w.Code, w.Body)
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
	ticket := a.ticket(u.ID, tomorrow().Unix(), 30)
	if _, _, _, e := a.verifyTicket(ticket, u); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e := a.verifyTicket(ticket+"x", u); e == nil {
		t.Fatal("tampered ticket accepted")
	}
	u.Duration = 60
	if _, _, _, e := a.verifyTicket(ticket, u); e == nil {
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
	if e := g.Insert(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if e := g.Delete(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if posts != 1 {
		t.Fatal(posts)
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
	ticket := a.ticket(u.ID, tomorrow().Unix(), 30)
	if _, _, _, e = second.verifyTicket(ticket, u); e != nil {
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
func BenchmarkSlotGeneration(b *testing.B) {
	u := User{Timezone: "America/Los_Angeles", Days: "0123456", StartMin: 540, EndMin: 1020, Duration: 30, Horizon: 30}
	loc, _ := time.LoadLocation(u.Timezone)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	b.ReportAllocs()
	for b.Loop() {
		generateSlots(u, day, day.AddDate(0, 0, -1), nil)
	}
}
