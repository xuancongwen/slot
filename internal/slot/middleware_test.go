package slot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPortsCSRFAndUserIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	b := bookingFor(v, tomorrow())
	a.reserve(context.Background(), b)
	for _, path := range []string{"/login", "/register", "/oauth/callback", "/settings", "/types/new"} {
		if w := getRequest(a.publicHandler(), path); w.Code != 404 {
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
	w = getRequest(a.adminHandler(), "/", cookie)
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
			if got := getRequest(tc.h, tc.path).Header().Get("Referrer-Policy"); got != "same-origin" {
				t.Fatalf("Referrer-Policy = %q, want same-origin", got)
			}
		})
	}
}

func TestAdminUnderPublicOrigin(t *testing.T) {
	a, _ := testApp(t)
	h := a.siteHandler()
	if w := getRequest(h, "/admin"); w.Code != 301 || w.Header().Get("Location") != "/admin/" {
		t.Fatalf("/admin: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := getRequest(h, "/admin/"); w.Code != 303 || w.Header().Get("Location") != "/admin/login" {
		t.Fatalf("signed-out dashboard: %d %s", w.Code, w.Header().Get("Location"))
	}
	if body := getRequest(h, "/admin/register").Body.String(); !strings.Contains(body, `action="/admin/register"`) {
		t.Fatal("registration form should post under /admin")
	}
	form := url.Values{"name": {"Sam"}, "slug": {"sam"}, "email": {"sam@example.com"}, "password": {"a long test password"}}
	w := formRequest(h, "/admin/register", form, true)
	if w.Code != 303 || w.Header().Get("Location") != "/admin/" {
		t.Fatalf("register: %d %s", w.Code, w.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "slot_session" {
			session = c
		}
	}
	if session == nil || session.Path != "/admin" {
		t.Fatalf("session cookie should be scoped to /admin: %+v", session)
	}
	if body := getRequest(h, "/admin/", session).Body.String(); !strings.Contains(body, `action="/admin/settings"`) || !strings.Contains(body, `href="/admin/types/new"`) {
		t.Fatal("dashboard links should stay under /admin")
	}
	if w := getRequest(h, "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Open the booking link") {
		t.Fatalf("public root: %d", w.Code)
	}
	if a.oauth.RedirectURL != "http://localhost:8080/admin/oauth/callback" {
		t.Fatalf("OAuth callback %s", a.oauth.RedirectURL)
	}
}

func TestAdminHostedSeparately(t *testing.T) {
	a, e := New(Config{DataDir: t.TempDir(), PublicURL: "http://localhost:8080", AdminURL: "http://localhost:8081", HostAdminSeparately: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	if w := getRequest(a.siteHandler(), "/admin/login"); w.Code != 404 {
		t.Fatalf("admin reachable on the public listener: %d", w.Code)
	}
	if body := getRequest(a.adminHandler(), "/login").Body.String(); !strings.Contains(body, `action="/login"`) {
		t.Fatal("separate admin should serve its pages at the root")
	}
	if a.oauth.RedirectURL != "http://localhost:8081/oauth/callback" {
		t.Fatalf("OAuth callback %s", a.oauth.RedirectURL)
	}
}

func TestFavicons(t *testing.T) {
	a, _ := testApp(t)
	h := a.siteHandler()
	for _, tc := range []struct{ page, icon string }{
		{"/", "/static/favicon.svg"},
		{"/admin/login", "/static/favicon-admin.svg"},
	} {
		if body := getRequest(h, tc.page).Body.String(); !strings.Contains(body, `rel="icon" type="image/svg+xml" href="`+tc.icon+`"`) {
			t.Errorf("%s should link %s", tc.page, tc.icon)
		}
		if w := getRequest(h, tc.icon); w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" {
			t.Errorf("%s: %d %s", tc.icon, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

// Pages load links and forms over fetch, which default-src 'none' would block.
func TestCSPAllowsSameOriginFetch(t *testing.T) {
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
			if got := getRequest(tc.h, tc.path).Header().Get("Content-Security-Policy"); !strings.Contains(got, "connect-src 'self'") {
				t.Fatalf("Content-Security-Policy = %q, want connect-src 'self'", got)
			}
		})
	}
}
