package slot

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

func (a *App) oauthStart(w http.ResponseWriter, r *http.Request) {
	if a.cfg.GoogleClientID == "" {
		a.fail(w, r, http.StatusBadRequest, "Google OAuth is not configured. Set the Google client environment variables and restart.")
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
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent select_account")), http.StatusSeeOther)
}

func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	cookie, _ := r.Cookie("slot_session")
	var verifier string
	e := a.db.QueryRowContext(r.Context(), `DELETE FROM oauth_states WHERE state=? AND user_id=? AND session=? AND expires>? RETURNING verifier`, hashToken(r.URL.Query().Get("state")), u.ID, hashToken(cookie.Value), time.Now().Unix()).Scan(&verifier)
	if e != nil {
		a.fail(w, r, http.StatusBadRequest, "Google connection expired or belongs to a different session. Please try again.")
		return
	}
	if r.URL.Query().Get("error") != "" {
		a.fail(w, r, http.StatusBadRequest, "Google connection was not approved. Please try again when ready.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, a.http)
	t, e := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if e != nil {
		a.fail(w, r, http.StatusBadGateway, "Google authorization could not be completed. Check the client configuration and try again.")
		return
	}
	if t.RefreshToken == "" {
		a.fail(w, r, http.StatusBadRequest, "Google did not grant offline access. Reconnect and approve all requested permissions.")
		return
	}
	g := a.google.(*Google)
	cs, e := g.list(ctx, t.AccessToken)
	if e != nil {
		a.fail(w, r, http.StatusBadGateway, "Calendar list unavailable. Approve all requested Google Calendar permissions and reconnect.")
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
		a.fail(w, r, http.StatusBadRequest, "No primary Google Calendar was found.")
		return
	}
	data, e := json.Marshal(t)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	e = a.inTx(ctx, func(tx *sql.Tx) error {
		var account int64
		if e := tx.QueryRowContext(ctx, `INSERT INTO accounts(user_id,identity,token) VALUES(?,?,?) ON CONFLICT(user_id,identity) DO UPDATE SET token=excluded.token RETURNING id`, u.ID, identity, a.seal(data)).Scan(&account); e != nil {
			return e
		}
		return saveCalendars(ctx, tx, account, cs)
	})
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=connected", http.StatusSeeOther)
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
		a.fail(w, r, http.StatusBadRequest, "Invalid account")
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
		a.fail(w, r, http.StatusBadGateway, "Reconnect this Google account.")
		return
	}
	cs, e := g.list(r.Context(), token)
	if e != nil {
		a.fail(w, r, http.StatusBadGateway, "Could not refresh calendars. Please try again.")
		return
	}
	e = a.inTx(r.Context(), func(tx *sql.Tx) error { return saveCalendars(r.Context(), tx, id, cs) })
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=refreshed", http.StatusSeeOther)
}
