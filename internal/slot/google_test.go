package slot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestGoogleBusyErrorsAndBatches(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	var calls int
	fail := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing bearer")
		}
		if r.URL.Path != "/freeBusy" {
			t.Error(r.URL.Path)
		}
		var body struct{ Items []struct{ ID string } }
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Items) > 50 {
			t.Error("exceeded Google batch size")
		}
		calls++
		result := map[string]any{}
		for _, item := range body.Items {
			if fail {
				result[item.ID] = map[string]any{"errors": []any{map[string]string{"reason": "notFound"}}}
			} else {
				result[item.ID] = map[string]any{"busy": []any{map[string]string{"start": "2026-10-05T09:00:00Z", "end": "2026-10-05T10:00:00Z"}}}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"calendars": result})
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	cs := make([]Calendar, 51)
	for i := range cs {
		cs[i] = c
		cs[i].GoogleID = fmt.Sprint(i)
	}
	busy, e := g.Busy(context.Background(), cs, tomorrow(), tomorrow().Add(time.Hour))
	if e != nil || calls != 2 || len(busy) != 51 {
		t.Fatalf("batching: %d %d %v", calls, len(busy), e)
	}
	fail = true
	if _, e = g.Busy(context.Background(), cs[:1], tomorrow(), tomorrow().Add(time.Hour)); e == nil {
		t.Fatal("partial Google error ignored")
	}
}

func TestGoogleInsertConflictAndDelete(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	b := bookingFor(u, tomorrow())
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			posts++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != b.ID || r.URL.Query().Get("sendUpdates") != "all" {
				t.Error("event ID/invitation missing")
			}
			w.WriteHeader(409)
		case "GET":
			json.NewEncoder(w).Encode(map[string]any{"status": "confirmed", "extendedProperties": map[string]any{"private": map[string]string{"slotBooking": b.ID}}})
		case "DELETE":
			w.WriteHeader(410)
		}
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	if _, e := g.Insert(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if e := g.Delete(context.Background(), c, b); e != nil {
		t.Fatal(e)
	}
	if posts != 1 {
		t.Fatal(posts)
	}
}

func TestGoogleInsertRequestsMeet(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	b := bookingFor(u, tomorrow())
	b.Meet = true
	b.Reason = "Plan the launch"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Description    string
			ConferenceData struct {
				CreateRequest struct {
					RequestID             string
					ConferenceSolutionKey struct{ Type string }
				}
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		req := body.ConferenceData.CreateRequest
		if r.URL.Query().Get("conferenceDataVersion") != "1" || req.RequestID != b.ID || req.ConferenceSolutionKey.Type != "hangoutsMeet" {
			t.Errorf("Meet not requested: %s %+v", r.URL.RawQuery, req)
		}
		if !strings.HasPrefix(body.Description, "Plan the launch\n\nManage or cancel: ") {
			t.Errorf("description %q", body.Description)
		}
		json.NewEncoder(w).Encode(map[string]string{"hangoutLink": fakeMeetLink})
	}))
	defer s.Close()
	g := &Google{app: a, baseURL: s.URL}
	link, e := g.Insert(context.Background(), c, b)
	if e != nil || link != fakeMeetLink {
		t.Fatalf("link %q, error %v", link, e)
	}
}

func TestTokenRefreshEncryptedAndPersistent(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	c, _ := a.bookingCalendar(context.Background(), u.WriteCalendar.Int64)
	old := oauth2.Token{AccessToken: "expired", RefreshToken: "refresh-secret", Expiry: time.Now().Add(-time.Hour)}
	data, _ := json.Marshal(old)
	a.db.Exec("UPDATE accounts SET token=? WHERE id=?", a.seal(data), c.AccountID)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh-secret" {
			t.Error("missing refresh token")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh-secret","token_type":"Bearer","expires_in":3600}`)
	}))
	defer s.Close()
	a.oauth.Endpoint.TokenURL = s.URL
	g := &Google{app: a}
	token, e := g.token(context.Background(), c.AccountID)
	if e != nil || token != "fresh-secret" {
		t.Fatalf("refresh: %s %v", token, e)
	}
	var encrypted []byte
	a.db.QueryRow("SELECT token FROM accounts WHERE id=?", c.AccountID).Scan(&encrypted)
	if strings.Contains(string(encrypted), "secret") {
		t.Fatal("plaintext secret")
	}
	data, e = a.open(encrypted)
	if e != nil || !strings.Contains(string(data), "refresh-secret") {
		t.Fatal("refresh token not preserved")
	}
}
