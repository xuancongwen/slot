package slot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type CalendarProvider interface {
	Busy(context.Context, []Calendar, time.Time, time.Time) ([]Span, error)
	// Insert returns the Google Meet link when the booking asked for one.
	Insert(context.Context, Calendar, Booking) (string, error)
	Delete(context.Context, Calendar, Booking) error
	Check(context.Context, Calendar, Booking) (EventState, error)
}

// EventState is what Google now holds for a confirmed booking's event.
type EventState struct {
	// Gone means the event was deleted in Google Calendar.
	Gone     bool
	Declined bool
	// Start and End are zero when the event has no time of day, such as an all-day event.
	Start, End int64
}

type Google struct {
	app     *App
	baseURL string
	tokenMu sync.Mutex
}

type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("Google Calendar returned HTTP %d", e.Status) }

func (g *Google) token(ctx context.Context, account int64) (string, error) {
	g.tokenMu.Lock()
	defer g.tokenMu.Unlock()
	var encrypted []byte
	if e := g.app.db.QueryRowContext(ctx, "SELECT token FROM accounts WHERE id=?", account).Scan(&encrypted); e != nil {
		return "", e
	}
	data, e := g.app.open(encrypted)
	if e != nil {
		return "", e
	}
	var old oauth2.Token
	if e = json.Unmarshal(data, &old); e != nil {
		return "", e
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.app.http)
	t, e := g.app.oauth.TokenSource(ctx, &old).Token()
	if e != nil {
		return "", errors.New("Google authorization expired or unavailable; reconnect this account")
	}
	if t.AccessToken != old.AccessToken || t.RefreshToken != old.RefreshToken {
		if t.RefreshToken == "" {
			t.RefreshToken = old.RefreshToken
		}
		data, e = json.Marshal(t)
		if e != nil {
			return "", e
		}
		if _, e = g.app.db.ExecContext(ctx, "UPDATE accounts SET token=? WHERE id=?", g.app.seal(data), account); e != nil {
			return "", e
		}
	}
	return t.AccessToken, nil
}

func (g *Google) request(ctx context.Context, token, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := g.app.http.Do(req)
	if e != nil {
		return errors.New("Google Calendar could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{resp.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

type remoteCalendar struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	AccessRole string `json:"accessRole"`
	Primary    bool   `json:"primary"`
}

func (g *Google) list(ctx context.Context, token string) ([]remoteCalendar, error) {
	var result []remoteCalendar
	next := ""
	for {
		var page struct {
			Items         []remoteCalendar `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}
		path := "/users/me/calendarList?maxResults=250"
		if next != "" {
			path += "&pageToken=" + url.QueryEscape(next)
		}
		if e := g.request(ctx, token, "GET", path, nil, &page); e != nil {
			return nil, e
		}
		result = append(result, page.Items...)
		next = page.NextPageToken
		if next == "" {
			break
		}
		if len(result) > 2000 {
			return nil, errors.New("too many calendars")
		}
	}
	return result, nil
}

func (g *Google) Busy(ctx context.Context, cs []Calendar, from, to time.Time) ([]Span, error) {
	groups := map[int64][]Calendar{}
	for _, c := range cs {
		groups[c.AccountID] = append(groups[c.AccountID], c)
	}
	// Parallelize independent accounts with a small bound, without caching stale availability.
	type result struct {
		busy []Span
		err  error
	}
	ch := make(chan result, len(groups))
	sem := make(chan struct{}, 4)
	for account, calendars := range groups {
		go func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				ch <- result{err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			b, e := g.busyAccount(ctx, account, calendars, from, to)
			ch <- result{b, e}
		}()
	}
	var all []Span
	var first error
	for range groups {
		r := <-ch
		if r.err != nil && first == nil {
			first = r.err
		}
		all = append(all, r.busy...)
	}
	return all, first
}

func (g *Google) busyAccount(ctx context.Context, account int64, cs []Calendar, from, to time.Time) ([]Span, error) {
	token, e := g.token(ctx, account)
	if e != nil {
		return nil, e
	}
	var all []Span
	for begin := 0; begin < len(cs); begin += 50 {
		end := min(begin+50, len(cs))
		items := []map[string]string{}
		for _, c := range cs[begin:end] {
			items = append(items, map[string]string{"id": c.GoogleID})
		}
		body := map[string]any{"timeMin": from.UTC().Format(time.RFC3339), "timeMax": to.UTC().Format(time.RFC3339), "items": items}
		var response struct {
			Calendars map[string]struct {
				Busy   []struct{ Start, End time.Time }
				Errors []json.RawMessage
			}
		}
		if e = g.request(ctx, token, "POST", "/freeBusy", body, &response); e != nil {
			return nil, e
		}
		for _, c := range cs[begin:end] {
			r, ok := response.Calendars[c.GoogleID]
			if !ok || len(r.Errors) > 0 {
				return nil, errors.New("a selected calendar could not be checked; reconnect or update calendar selections")
			}
			for _, b := range r.Busy {
				if !b.End.After(b.Start) {
					return nil, errors.New("invalid free/busy interval")
				}
				all = append(all, Span{b.Start, b.End})
			}
		}
	}
	return all, nil
}

func (g *Google) Insert(ctx context.Context, c Calendar, b Booking) (string, error) {
	token, e := g.token(ctx, c.AccountID)
	if e != nil {
		return "", e
	}
	path := "/calendars/" + url.PathEscape(c.GoogleID) + "/events"
	// Caller-supplied Google event IDs make retries safe after a timeout or process restart.
	description := "Manage or cancel: " + g.app.cfg.PublicURL + "/manage/" + b.ManageToken
	if b.Reason != "" {
		description = b.Reason + "\n\n" + description
	}
	body := map[string]any{"id": b.ID, "summary": b.Title, "location": b.Location, "description": description, "start": map[string]string{"dateTime": time.Unix(b.Start, 0).UTC().Format(time.RFC3339), "timeZone": b.Timezone}, "end": map[string]string{"dateTime": time.Unix(b.End, 0).UTC().Format(time.RFC3339), "timeZone": b.Timezone}, "attendees": []map[string]string{{"email": b.GuestEmail, "displayName": b.GuestName}}, "extendedProperties": map[string]any{"private": map[string]string{"slotBooking": b.ID}}, "guestsCanModify": false}
	query := "?sendUpdates=all"
	if b.Meet {
		// Reusing the booking ID as the request ID keeps a retried insert to one Meet.
		body["conferenceData"] = map[string]any{"createRequest": map[string]any{"requestId": b.ID, "conferenceSolutionKey": map[string]string{"type": "hangoutsMeet"}}}
		query += "&conferenceDataVersion=1"
	}
	var created struct{ HangoutLink string }
	e = g.request(ctx, token, "POST", path+query, body, &created)
	var api *APIError
	if errors.As(e, &api) && api.Status == 409 {
		var event struct {
			Status             string
			HangoutLink        string
			ExtendedProperties struct{ Private map[string]string }
		}
		if err := g.request(ctx, token, "GET", path+"/"+b.ID, nil, &event); err != nil {
			return "", err
		}
		if event.Status == "cancelled" {
			return "", errors.New("Google event was cancelled externally; cancel this booking in Slot")
		}
		if event.ExtendedProperties.Private["slotBooking"] != b.ID {
			return "", errors.New("Google event ID conflict")
		}
		return event.HangoutLink, nil
	}
	return created.HangoutLink, e
}

func (g *Google) Delete(ctx context.Context, c Calendar, b Booking) error {
	token, e := g.token(ctx, c.AccountID)
	if e != nil {
		return e
	}
	e = g.request(ctx, token, "DELETE", "/calendars/"+url.PathEscape(c.GoogleID)+"/events/"+b.ID+"?sendUpdates=all", nil, nil)
	var api *APIError
	if errors.As(e, &api) && (api.Status == 404 || api.Status == 410) {
		return nil
	}
	return e
}

// Check reads the booking's event back, so edits made in Google Calendar reach Slot.
func (g *Google) Check(ctx context.Context, c Calendar, b Booking) (EventState, error) {
	token, e := g.token(ctx, c.AccountID)
	if e != nil {
		return EventState{}, e
	}
	path := "/calendars/" + url.PathEscape(c.GoogleID)
	var event struct {
		Status     string
		Start, End struct{ DateTime time.Time }
		Attendees  []struct{ Email, ResponseStatus string }
	}
	e = g.request(ctx, token, "GET", path+"/events/"+b.ID, nil, &event)
	var api *APIError
	if errors.As(e, &api) && (api.Status == 404 || api.Status == 410) {
		// A calendar that is gone or no longer shared also answers 404. Its events may
		// still exist, so only a reachable calendar proves the event was deleted.
		if err := g.request(ctx, token, "GET", path, nil, nil); err != nil {
			return EventState{}, fmt.Errorf("checking destination calendar: %w", err)
		}
		return EventState{Gone: true}, nil
	}
	if e != nil {
		return EventState{}, e
	}
	s := EventState{Gone: event.Status == "cancelled"}
	if !event.Start.DateTime.IsZero() && !event.End.DateTime.IsZero() {
		s.Start, s.End = event.Start.DateTime.Unix(), event.End.DateTime.Unix()
	}
	for _, a := range event.Attendees {
		if strings.EqualFold(a.Email, b.GuestEmail) && a.ResponseStatus == "declined" {
			s.Declined = true
		}
	}
	return s, nil
}
