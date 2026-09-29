package slot

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestMeetingTypeEditingAndIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	theirs := chatType(t, a, seedHost(t, a, "two"))
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	form := func(slug, tz string) url.Values {
		return url.Values{"name": {"Deep dive"}, "slug": {slug}, "timezone": {tz}, "duration": {"60"}, "days": {"1", "2"}, "start": {"09:00"}, "end": {"17:00"}, "buffer": {"0"}, "notice": {"0"}, "horizon": {"30"}, "active": {"on"}, "approval": {"on"}}
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
	var approval bool
	a.db.QueryRow("SELECT timezone,days,approval FROM meeting_types WHERE user_id=? AND slug='deep'", u.ID).Scan(&zone, &days, &approval)
	if zone != "Asia/Singapore" || days != "12" || !approval {
		t.Fatalf("saved %q %q %v", zone, days, approval)
	}
	var n int
	a.db.QueryRow("SELECT count(*) FROM meeting_types WHERE id=? AND slug='chat'", theirs.ID).Scan(&n)
	if n != 1 {
		t.Fatal("another host's meeting type was changed or deleted")
	}
}

func TestMeetingTypeActiveToggle(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	theirs := chatType(t, a, seedHost(t, a, "two"))
	cookie := sessionFor(t, a, u)
	mine := chatType(t, a, u)
	for _, tc := range []struct {
		name         string
		id           int64
		before       bool
		days, active string
		wantCode     int
		wantActive   bool
	}{
		{"turn off", mine.ID, true, "12345", "0", 303, false},
		{"turn on", mine.ID, false, "12345", "1", 303, true},
		{"turn on without weekdays", mine.ID, false, "", "1", 400, false},
		{"another host's type", theirs.ID, true, "12345", "0", 404, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := a.db.Exec("UPDATE meeting_types SET active=?,days=? WHERE id=?", tc.before, tc.days, tc.id); e != nil {
				t.Fatal(e)
			}
			w := formRequest(a.adminHandler(), fmt.Sprintf("/types/%d/active", tc.id), url.Values{"active": {tc.active}}, true, cookie)
			if w.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantCode, w.Body)
			}
			var active bool
			a.db.QueryRow("SELECT active FROM meeting_types WHERE id=?", tc.id).Scan(&active)
			if active != tc.wantActive {
				t.Fatalf("active %v, want %v", active, tc.wantActive)
			}
		})
	}
}

func TestMeetingTypeLocations(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	theirs := locationID(t, a, seedHost(t, a, "two"), "Zoom")
	cookie := sessionFor(t, a, u)
	zoom := locationID(t, a, u, "Zoom")
	for _, tc := range []struct {
		name      string
		form      url.Values
		wantCode  int
		wantSaved string
	}{
		{"all locations", url.Values{"location_mode": {"all"}, "guest_location": {"on"}}, 303, "all=true guest=true picks=[]"},
		{"only Zoom", url.Values{"location_mode": {"some"}, "locations": {zoom}}, 303, "all=false guest=false picks=[" + zoom + "]"},
		{"only the guest's", url.Values{"location_mode": {"some"}, "guest_location": {"on"}}, 303, "all=false guest=true picks=[]"},
		{"nothing offered", url.Values{"location_mode": {"some"}}, 400, ""},
		{"another host's location", url.Values{"location_mode": {"some"}, "locations": {zoom, theirs}}, 303, "all=false guest=false picks=[" + zoom + "]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"name": {"Chat"}, "slug": {"chat"}, "timezone": {"UTC"}, "duration": {"30"}, "days": {"1"}, "start": {"09:00"}, "end": {"17:00"}, "buffer": {"0"}, "notice": {"0"}, "horizon": {"30"}}
			for k, v := range tc.form {
				form[k] = v
			}
			mt := chatType(t, a, u)
			if w := formRequest(a.adminHandler(), fmt.Sprintf("/types/%d", mt.ID), form, true, cookie); w.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.wantCode, w.Body)
			}
			if tc.wantSaved == "" {
				return
			}
			mt = chatType(t, a, u)
			picks, _ := queryAll(t.Context(), a.db, scanString, "SELECT location_id FROM meeting_type_locations WHERE meeting_type_id=?", mt.ID)
			if got := fmt.Sprintf("all=%v guest=%v picks=%v", mt.AllLocations, mt.GuestLocation, picks); got != tc.wantSaved {
				t.Fatalf("saved %s, want %s", got, tc.wantSaved)
			}
		})
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

func TestDashboardTypeTitleOpensEditor(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	body := getRequest(a.adminHandler(), "/", sessionFor(t, a, u)).Body.String()
	if !strings.Contains(body, fmt.Sprintf(`<a class="type-name" href="/admin/types/%d">Chat</a>`, mt.ID)) {
		t.Fatalf("title does not link to the editor: %s", body)
	}
	if strings.Contains(body, ">Edit</a>") {
		t.Fatal("separate Edit link still shown")
	}
}
