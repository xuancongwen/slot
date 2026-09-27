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
