package slot

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGuestLocationChoice(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	theirs := seedHost(t, a, "two")
	all := chatType(t, a, u)
	zoomOnly := restrictedType(t, a, u, "zoom-only", false, "Zoom")
	zoomOrGuest := restrictedType(t, a, u, "zoom-or-guest", true, "Zoom")
	nowhere := restrictedType(t, a, u, "nowhere", false)
	for _, tc := range []struct {
		name, choice, custom, want string
		mt                         MeetingType
		meet, fails                bool
	}{
		{name: "Google Meet", mt: all, choice: locationID(t, a, u, "Google Meet"), meet: true},
		{name: "host link", mt: all, choice: locationID(t, a, u, "Zoom"), want: "https://zoom.us/j/1"},
		{name: "guest's own text wins", mt: all, choice: locationID(t, a, u, "Zoom"), custom: " Call me at 555-0100 ", want: "Call me at 555-0100"},
		{name: "nothing chosen", mt: all},
		{name: "another host's location", mt: all, choice: locationID(t, a, theirs, "Zoom"), fails: true},
		{name: "too long", mt: all, custom: strings.Repeat("x", 501), fails: true},
		{name: "offered location", mt: zoomOnly, choice: locationID(t, a, u, "Zoom"), want: "https://zoom.us/j/1"},
		{name: "location not offered", mt: zoomOnly, choice: locationID(t, a, u, "Google Meet"), fails: true},
		{name: "guest text not offered", mt: zoomOnly, choice: locationID(t, a, u, "Zoom"), custom: "My office", fails: true},
		{name: "offered choice skipped", mt: zoomOnly, fails: true},
		{name: "guest text offered", mt: zoomOrGuest, custom: "My office", want: "My office"},
		{name: "nothing offered", mt: nowhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.PostForm = url.Values{"location": {tc.choice}, "custom_location": {tc.custom}}
			text, meet, e := a.chosenLocation(r, u, tc.mt)
			if (e != nil) != tc.fails || text != tc.want || meet != tc.meet {
				t.Fatalf("got %q meet=%v err=%v", text, meet, e)
			}
		})
	}
}

func TestBookingPageOffersTypeLocations(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	restrictedType(t, a, u, "zoom-only", false, "Zoom")
	start := tomorrow()
	path := fmt.Sprintf("/b/alex/zoom-only?tz=UTC&month=%s&date=%s&start=%d", start.Format("2006-01"), start.Format(dateLayout), start.Unix())
	w := getRequest(a.publicHandler(), path)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "zoom.us") || strings.Contains(body, "Google Meet") || strings.Contains(body, "custom_location") {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	if !strings.Contains(body, `value="`+locationID(t, a, u, "Zoom")+`" checked`) {
		t.Fatal("the only offered location is not preselected")
	}
}

func TestLocationEditingIsolation(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	theirs := locationID(t, a, v, "Zoom")
	formRequest(h, "/locations/"+theirs+"/delete", url.Values{}, true, cookie)
	formRequest(h, "/locations/"+theirs+"/default", url.Values{}, true, cookie)
	var n int
	a.db.QueryRow("SELECT count(*) FROM locations WHERE id=?", theirs).Scan(&n)
	if n != 1 {
		t.Fatal("deleted another host's location")
	}
	if got, _ := a.userByID(context.Background(), u.ID); got.DefaultLocation != u.DefaultLocation {
		t.Fatalf("default changed to %v", got.DefaultLocation)
	}
	if w := formRequest(h, "/locations", url.Values{"label": {"Office"}, "detail": {"1 Main St"}}, true, cookie); w.Code != 303 {
		t.Fatalf("add: %d", w.Code)
	}
	office := locationID(t, a, u, "Office")
	formRequest(h, "/locations/"+office+"/default", url.Values{}, true, cookie)
	if got, _ := a.userByID(context.Background(), u.ID); fmt.Sprint(got.DefaultLocation.Int64) != office {
		t.Fatal("default not changed")
	}
}

func TestReorderLocations(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "one")
	v := seedHost(t, a, "two")
	cookie := sessionFor(t, a, u)
	h := a.adminHandler()
	order := func(u User) string {
		ls, e := a.locations(context.Background(), u)
		if e != nil {
			t.Fatal(e)
		}
		var labels []string
		for _, l := range ls {
			labels = append(labels, l.Label)
		}
		return fmt.Sprint(labels)
	}
	formRequest(h, "/locations", url.Values{"label": {"Office"}}, true, cookie)
	for _, tc := range []struct{ label, direction, want string }{
		{"Office", "up", "[Google Meet Office Zoom]"},
		{"Office", "up", "[Office Google Meet Zoom]"},
		{"Office", "up", "[Office Google Meet Zoom]"},
		{"Google Meet", "down", "[Office Zoom Google Meet]"},
	} {
		formRequest(h, "/locations/"+locationID(t, a, u, tc.label)+"/move", url.Values{"direction": {tc.direction}}, true, cookie)
		if got := order(u); got != tc.want {
			t.Fatalf("after moving %s %s: %s, want %s", tc.label, tc.direction, got, tc.want)
		}
	}
	formRequest(h, "/locations/"+locationID(t, a, v, "Zoom")+"/move", url.Values{"direction": {"up"}}, true, cookie)
	if got := order(v); got != "[Google Meet Zoom]" {
		t.Fatalf("another host's locations reordered: %s", got)
	}
}
