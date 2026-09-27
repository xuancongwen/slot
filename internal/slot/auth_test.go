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
