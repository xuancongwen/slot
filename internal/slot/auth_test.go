package slot

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

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

func TestSingleHostRegistersOnce(t *testing.T) {
	a, _ := testApp(t)
	a.cfg.SingleHost = "sam"
	a.cfg.RegistrationCode = "let-me-in"
	h := a.adminHandler()
	form := func(slug, email, code string) url.Values {
		return url.Values{"name": {"Sam"}, "slug": {slug}, "email": {email}, "password": {"a long test password"}, "code": {code}}
	}
	if body := getRequest(h, "/register").Body.String(); !strings.Contains(body, `value="sam" readonly`) {
		t.Fatal("registration form should fix the URL name to SINGLE_HOST")
	}
	for _, tc := range []struct {
		name string
		form url.Values
		want int
	}{
		{"wrong code", form("sam", "sam@example.com", "nope"), 403},
		{"another URL name", form("alex", "sam@example.com", "let-me-in"), 400},
		{"the single host", form("sam", "sam@example.com", "let-me-in"), 303},
		{"a second account", form("sam", "two@example.com", "let-me-in"), 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := formRequest(h, "/register", tc.form, true); w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
	if w := getRequest(h, "/register"); w.Code != 403 {
		t.Fatalf("registration page still open: %d", w.Code)
	}
	if body := getRequest(h, "/login").Body.String(); strings.Contains(body, "Create an account") {
		t.Fatal("sign-in page still offers registration")
	}
}
