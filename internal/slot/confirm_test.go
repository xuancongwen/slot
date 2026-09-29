package slot

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMail struct{ to, subject, body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	err  error
}

func (f *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

var linkPattern = regexp.MustCompile(`http://localhost:8080(/manage/[0-9a-f]+)\?confirm=([0-9a-f]+)`)

// bookChat submits the booking form for alex's chat type at start.
func bookChat(t *testing.T, a *App, u User, start time.Time, email string) (code int, manage string) {
	t.Helper()
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), start.Unix())}, "name": {"Guest"}, "email": {email}}
	w := formRequest(a.publicHandler(), "/b/"+u.Slug+"/chat", form, false)
	return w.Code, w.Header().Get("Location")
}

func TestGuestConfirmsByEmail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		approval   bool
		wantStatus string
	}{
		{"instant booking", false, "confirmed"},
		{"request", true, "requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f := testApp(t)
			m := &fakeMailer{}
			a.mail = m
			u := seedHost(t, a, "alex")
			if _, e := a.db.Exec("UPDATE meeting_types SET approval=? WHERE user_id=?", tc.approval, u.ID); e != nil {
				t.Fatal(e)
			}
			code, manage := bookChat(t, a, u, tomorrow(), "guest@example.com")
			if code != 303 || len(m.sent) != 1 || m.sent[0].to != "guest@example.com" {
				t.Fatalf("book: %d, sent %+v", code, m.sent)
			}
			link := linkPattern.FindStringSubmatch(m.sent[0].body)
			if link == nil || link[1] != manage {
				t.Fatalf("email has no confirmation link for %s: %s", manage, m.sent[0].body)
			}
			a.reconcile(context.Background())
			if bs, _ := a.hostBookings(context.Background(), u.ID); len(bs) != 0 || f.insertCalls != 0 {
				t.Fatalf("unconfirmed booking reached the host: %d listed, %d inserts", len(bs), f.insertCalls)
			}
			if e := a.reserve(context.Background(), bookingFor(u, tomorrow())); e == nil {
				t.Fatal("unconfirmed booking did not hold its time")
			}
			h := a.publicHandler()
			if body := getRequest(h, manage).Body.String(); !strings.Contains(body, "Check your<br>email") || strings.Contains(body, link[2]) {
				t.Fatalf("booking page must not reveal the code: %s", body)
			}
			if body := getRequest(h, manage+"?confirm="+link[2]).Body.String(); !strings.Contains(body, "Confirm booking") {
				t.Fatalf("link page: %s", body)
			}
			if w := formRequest(h, manage+"/confirm", url.Values{"code": {strings.Repeat("0", 64)}}, false); w.Code != 400 {
				t.Fatalf("wrong code: %d", w.Code)
			}
			for range 2 {
				if w := formRequest(h, manage+"/confirm", url.Values{"code": {link[2]}}, false); w.Code != 303 {
					t.Fatalf("confirm: %d %s", w.Code, w.Body)
				}
			}
			a.reconcile(context.Background())
			b, _ := a.getBooking(context.Background(), strings.TrimPrefix(manage, "/manage/"))
			if !b.Verified || b.Status != tc.wantStatus {
				t.Fatalf("verified %v, status %s", b.Verified, b.Status)
			}
			if bs, _ := a.hostBookings(context.Background(), u.ID); len(bs) != 1 {
				t.Fatal("confirmed booking missing from the host's list")
			}
		})
	}
}

func TestUnconfirmedBookingReleased(t *testing.T) {
	a, f := testApp(t)
	m := &fakeMailer{}
	a.mail = m
	u := seedHost(t, a, "alex")
	_, manage := bookChat(t, a, u, tomorrow(), "guest@example.com")
	code := linkPattern.FindStringSubmatch(m.sent[0].body)[2]
	if _, e := a.db.Exec("UPDATE bookings SET created=?", time.Now().Add(-holdWindow-time.Second).Unix()); e != nil {
		t.Fatal(e)
	}
	h := a.publicHandler()
	if w := formRequest(h, manage+"/confirm", url.Values{"code": {code}}, false); w.Code != 409 {
		t.Fatalf("late confirmation: %d", w.Code)
	}
	a.reconcile(context.Background())
	if f.insertCalls != 0 {
		t.Fatal("unconfirmed booking reached Google")
	}
	if e := a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil {
		t.Fatal("expired hold kept its time:", e)
	}
	if body := getRequest(h, manage+"?confirm="+code).Body.String(); !strings.Contains(body, "Booking<br>released") || strings.Contains(body, "Confirm booking") {
		t.Fatalf("released page: %s", body)
	}
	if _, e := a.db.Exec("UPDATE bookings SET created=? WHERE verified=0", time.Now().Add(-25*time.Hour).Unix()); e != nil {
		t.Fatal(e)
	}
	a.reconcile(context.Background())
	if w := getRequest(h, manage); w.Code != 404 {
		t.Fatalf("old unconfirmed booking kept: %d", w.Code)
	}
}

func TestGuestCancelsUnconfirmedBooking(t *testing.T) {
	a, f := testApp(t)
	a.mail = &fakeMailer{}
	u := seedHost(t, a, "alex")
	_, manage := bookChat(t, a, u, tomorrow(), "guest@example.com")
	if w := formRequest(a.publicHandler(), manage+"/cancel", url.Values{}, false); w.Code != 303 {
		t.Fatal(w.Code)
	}
	a.reconcile(context.Background())
	if e := a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil || f.deleteCalls != 0 {
		t.Fatalf("cancel did not release the time at once: %v, %d deletes", e, f.deleteCalls)
	}
}

func TestConfirmationEmailsLimitedPerAddress(t *testing.T) {
	a, _ := testApp(t)
	m := &fakeMailer{}
	a.mail = m
	u := seedHost(t, a, "alex")
	for i, email := range []string{"guest@example.com", "Guest@example.com", "guest@EXAMPLE.com"} {
		if code, _ := bookChat(t, a, u, tomorrow().Add(time.Duration(i)*time.Hour), email); code != 303 {
			t.Fatalf("booking %d: %d", i, code)
		}
	}
	if code, _ := bookChat(t, a, u, tomorrow().Add(4*time.Hour), "GUEST@example.com"); code != 429 || len(m.sent) != maxUnconfirmed {
		t.Fatalf("fourth booking: %d, %d emails", code, len(m.sent))
	}
	if code, _ := bookChat(t, a, u, tomorrow().Add(4*time.Hour), "other@example.com"); code != 303 {
		t.Fatalf("another address: %d", code)
	}
}

func TestConfirmationEmailFailureReleasesTime(t *testing.T) {
	a, _ := testApp(t)
	a.mail = &fakeMailer{err: errors.New("550 no such user")}
	u := seedHost(t, a, "alex")
	if code, _ := bookChat(t, a, u, tomorrow(), "guest@example.com"); code != 502 {
		t.Fatalf("book: %d", code)
	}
	if e := a.reserve(context.Background(), bookingFor(u, tomorrow())); e != nil {
		t.Fatal("time held without a confirmation email:", e)
	}
}

func TestDisposableEmailCannotBook(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	for i, tc := range []struct {
		email string
		want  int
	}{
		{"guest@mailinator.com", 400},
		{"guest@MAILINATOR.com", 400},
		{"guest@inbox.mailinator.com", 400},
		{"guest@example.com", 303},
		{"guest@xyzmailinator.com", 303},
	} {
		t.Run(tc.email, func(t *testing.T) {
			if code, _ := bookChat(t, a, u, tomorrow().Add(time.Duration(i)*time.Hour), tc.email); code != tc.want {
				t.Fatalf("got %d, want %d", code, tc.want)
			}
		})
	}
}

func TestConfirmationEmailContent(t *testing.T) {
	a, _ := testApp(t)
	m := &fakeMailer{}
	a.mail = m
	u := seedHost(t, a, "alex")
	form := url.Values{"ticket": {a.ticket(u.ID, chatType(t, a, u), tomorrow().Unix())}, "name": {"Win $1000 at spam.example"}, "email": {"guest@example.com"}, "tz": {"Asia/Singapore"}}
	if w := formRequest(a.publicHandler(), "/b/alex/chat", form, false); w.Code != 303 {
		t.Fatal(w.Code)
	}
	body := m.sent[0].body
	if strings.Contains(body, "spam.example") {
		t.Fatal("guest-supplied name was emailed")
	}
	if want := tomorrow().In(mustLoad(t, "Asia/Singapore")).Format("15:04"); !strings.Contains(body, want) || !strings.Contains(body, strconv.Itoa(int(holdWindow.Minutes()))+" minutes") {
		t.Fatalf("email body: %s", body)
	}
}
