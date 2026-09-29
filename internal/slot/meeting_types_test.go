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
