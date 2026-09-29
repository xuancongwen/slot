package slot

import (
	"fmt"
	"net/url"
	"testing"
)

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
