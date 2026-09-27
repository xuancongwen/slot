package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type CalendarProvider interface {
	Busy(context.Context, []Calendar, time.Time, time.Time) ([]Span, error)
	// Insert returns the Google Meet link when the booking asked for one.
	Insert(context.Context, Calendar, Booking) (string, error)
	Delete(context.Context, Calendar, Booking) error
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
	body := map[string]any{"id": b.ID, "summary": b.Title, "location": b.Location, "description": "Manage or cancel: " + g.app.cfg.PublicURL + "/manage/" + b.ManageToken, "start": map[string]string{"dateTime": time.Unix(b.Start, 0).UTC().Format(time.RFC3339), "timeZone": b.Timezone}, "end": map[string]string{"dateTime": time.Unix(b.End, 0).UTC().Format(time.RFC3339), "timeZone": b.Timezone}, "attendees": []map[string]string{{"email": b.GuestEmail, "displayName": b.GuestName}}, "extendedProperties": map[string]any{"private": map[string]string{"slotBooking": b.ID}}, "guestsCanModify": false}
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

func (a *App) oauthStart(w http.ResponseWriter, r *http.Request) {
	if a.cfg.GoogleClientID == "" {
		a.fail(w, r, 400, "Google OAuth is not configured. Set the Google client environment variables and restart.")
		return
	}
	u := currentUser(r)
	session, _ := r.Cookie("slot_session")
	state := randomHex(32)
	verifier := oauth2.GenerateVerifier()
	_, e := a.db.ExecContext(r.Context(), "INSERT INTO oauth_states VALUES(?,?,?,?,?)", hashToken(state), u.ID, hashToken(session.Value), verifier, time.Now().Add(10*time.Minute).Unix())
	if e != nil {
		a.internal(w, r, e)
		return
	}
	// OAuth crosses origins intentionally; this redirect is issued only after a CSRF-checked POST.
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent select_account")), 303)
}
func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	cookie, _ := r.Cookie("slot_session")
	var verifier string
	e := a.db.QueryRowContext(r.Context(), `DELETE FROM oauth_states WHERE state=? AND user_id=? AND session=? AND expires>? RETURNING verifier`, hashToken(r.URL.Query().Get("state")), u.ID, hashToken(cookie.Value), time.Now().Unix()).Scan(&verifier)
	if e != nil {
		a.fail(w, r, 400, "Google connection expired or belongs to a different session. Please try again.")
		return
	}
	if r.URL.Query().Get("error") != "" {
		a.fail(w, r, 400, "Google connection was not approved. Please try again when ready.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, a.http)
	t, e := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if e != nil {
		a.fail(w, r, 502, "Google authorization could not be completed. Check the client configuration and try again.")
		return
	}
	if t.RefreshToken == "" {
		a.fail(w, r, 400, "Google did not grant offline access. Reconnect and approve all requested permissions.")
		return
	}
	g := a.google.(*Google)
	cs, e := g.list(ctx, t.AccessToken)
	if e != nil {
		a.fail(w, r, 502, "Calendar list unavailable. Approve all requested Google Calendar permissions and reconnect.")
		return
	}
	identity := ""
	for _, c := range cs {
		if c.Primary {
			identity = c.ID
			break
		}
	}
	if identity == "" {
		a.fail(w, r, 400, "No primary Google Calendar was found.")
		return
	}
	data, e := json.Marshal(t)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	tx, e := a.db.BeginTx(ctx, nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	var account int64
	e = tx.QueryRowContext(ctx, `INSERT INTO accounts(user_id,identity,token) VALUES(?,?,?) ON CONFLICT(user_id,identity) DO UPDATE SET token=excluded.token RETURNING id`, u.ID, identity, a.seal(data)).Scan(&account)
	if e == nil {
		e = saveCalendars(ctx, tx, account, cs)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=connected", 303)
}
func saveCalendars(ctx context.Context, tx *sql.Tx, account int64, cs []remoteCalendar) error {
	// Keep missing rows for booking history, but remove their ability to accept writes.
	if _, e := tx.ExecContext(ctx, "UPDATE calendars SET role='unavailable' WHERE account_id=?", account); e != nil {
		return e
	}
	for _, c := range cs {
		if _, e := tx.ExecContext(ctx, `INSERT INTO calendars(account_id,google_id,name,role,check_busy) VALUES(?,?,?,?,?) ON CONFLICT(account_id,google_id) DO UPDATE SET name=excluded.name,role=excluded.role`, account, c.ID, c.Summary, c.AccessRole, c.Primary); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) refreshCalendars(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, e := strconv.ParseInt(r.PostForm.Get("account"), 10, 64)
	if e != nil {
		a.fail(w, r, 400, "Invalid account")
		return
	}
	var owner int64
	if e = a.db.QueryRowContext(r.Context(), "SELECT user_id FROM accounts WHERE id=?", id).Scan(&owner); e != nil || owner != u.ID {
		http.NotFound(w, r)
		return
	}
	g := a.google.(*Google)
	token, e := g.token(r.Context(), id)
	if e != nil {
		a.fail(w, r, 502, "Reconnect this Google account.")
		return
	}
	cs, e := g.list(r.Context(), token)
	if e != nil {
		a.fail(w, r, 502, "Could not refresh calendars. Please try again.")
		return
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	e = saveCalendars(r.Context(), tx, id, cs)
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=refreshed", 303)
}
