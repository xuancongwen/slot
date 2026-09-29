package slot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCalendar struct {
	mu                            sync.Mutex
	busy                          []Span
	busyErr, insertErr, deleteErr error
	checkErr                      error
	insertCalls, deleteCalls      int
	checkCalls                    int
	events                        map[string]bool
	// remote is what Check reports for an event that still exists.
	remote EventState
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

func (f *fakeCalendar) Check(_ context.Context, _ Calendar, b Booking) (EventState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checkErr != nil {
		return EventState{}, f.checkErr
	}
	if !f.events[b.ID] {
		return EventState{Gone: true}, nil
	}
	return f.remote, nil
}

func testApp(t *testing.T) (*App, *fakeCalendar) {
	t.Helper()
	a, e := New(Config{DataDir: t.TempDir(), PublicURL: "http://localhost:8080", AdminURL: "http://localhost:8081", RegistrationOpen: true})
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
	return Booking{ID: randomHex(16), UserID: u.ID, CalendarID: u.WriteCalendar.Int64, GuestName: "Guest", GuestEmail: "guest@example.com", Start: start.Unix(), End: start.Add(30 * time.Minute).Unix(), BlockStart: start.Unix(), BlockEnd: start.Add(30 * time.Minute).Unix(), Title: "Guest / Alex", Timezone: "UTC", ManageToken: randomHex(32), Status: "pending", Created: time.Now().Unix()}
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

func getRequest(h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
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
