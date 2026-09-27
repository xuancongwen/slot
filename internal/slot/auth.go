package slot

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

func passwordHash(password string) string {
	salt := randomHex(16)
	key, _ := pbkdf2.Key(sha256.New, password, []byte(salt), 600000, 32)
	return salt + ":" + hex.EncodeToString(key)
}

func passwordMatches(encoded, password string) bool {
	parts := strings.Split(encoded, ":")
	if len(parts) != 2 {
		return false
	}
	key, _ := pbkdf2.Key(sha256.New, password, []byte(parts[0]), 600000, 32)
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(key)), []byte(parts[1])) == 1
}

func hashToken(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type contextKey string

const userKey contextKey = "user"

func currentUser(r *http.Request) User { return r.Context().Value(userKey).(User) }

func (a *App) authenticated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie("slot_session")
		if e != nil {
			http.Redirect(w, r, "/login", 303)
			return
		}
		var uid int64
		e = a.db.QueryRowContext(r.Context(), "SELECT user_id FROM sessions WHERE token=? AND expires>?", hashToken(c.Value), time.Now().Unix()).Scan(&uid)
		if e != nil {
			http.Redirect(w, r, "/login", 303)
			return
		}
		u, e := a.userByID(r.Context(), uid)
		if e != nil {
			http.Error(w, "Account unavailable", 500)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

func (a *App) loginSession(w http.ResponseWriter, r *http.Request, id int64) error {
	token := randomHex(32)
	if c, e := r.Cookie("slot_session"); e == nil {
		a.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token=?", hashToken(c.Value))
	}
	_, e := a.db.ExecContext(r.Context(), "INSERT INTO sessions VALUES(?,?,?)", hashToken(token), id, time.Now().Add(7*24*time.Hour).Unix())
	if e == nil {
		a.cookie(w, "slot_session", token, 7*86400, true)
	}
	return e
}

func (a *App) authPage(w http.ResponseWriter, r *http.Request) {
	reg := r.URL.Path == "/register"
	if reg && !a.cfg.RegistrationOpen {
		a.fail(w, r, 403, "Registration is closed on this server.")
		return
	}
	title := "Sign in"
	if reg {
		title = "Create account"
	}
	a.render(w, r, "auth", Page{Title: title, Admin: true, Register: reg, RegistrationOpen: a.cfg.RegistrationOpen}, 200)
}

func (a *App) authCapacity(w http.ResponseWriter) bool {
	select {
	case a.authSlots <- struct{}{}:
		return true
	default:
		http.Error(w, "Please try again in a moment", 429)
		return false
	}
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.RegistrationOpen {
		a.fail(w, r, 403, "Registration is closed.")
		return
	}
	if a.cfg.RegistrationCode != "" && subtle.ConstantTimeCompare([]byte(r.PostForm.Get("code")), []byte(a.cfg.RegistrationCode)) != 1 {
		a.fail(w, r, 403, "The registration code is incorrect.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	name := strings.TrimSpace(r.PostForm.Get("name"))
	slug := strings.ToLower(strings.TrimSpace(r.PostForm.Get("slug")))
	password := r.PostForm.Get("password")
	if !validEmail(email) || len(name) < 1 || len(name) > 100 || !slugPattern.MatchString(slug) || len(password) < 12 || len(password) > 256 {
		a.fail(w, r, 400, "Use a valid email, a name up to 100 characters, a URL name of 1–40 lowercase letters/digits/hyphens, and a password of 12–256 characters.")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	hash := passwordHash(password)
	<-a.authSlots
	// The browser fills in its own timezone; without JavaScript the starter type uses UTC.
	tz := r.PostForm.Get("timezone")
	if !validTimezone(tz) {
		tz = "UTC"
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	var id int64
	e = tx.QueryRowContext(r.Context(), "INSERT INTO users(email,password,name,slug,created) VALUES(?,?,?,?,?) RETURNING id", email, hash, name, slug, time.Now().Unix()).Scan(&id)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			a.fail(w, r, 409, "That email or booking URL is already registered.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	_, e = tx.ExecContext(r.Context(), "INSERT INTO meeting_types(user_id,slug,name,timezone) VALUES(?,'30min','30 minute meeting',?)", id, tz)
	var meet int64
	if e == nil {
		e = tx.QueryRowContext(r.Context(), "INSERT INTO locations(user_id,kind,label) VALUES(?,'meet','Google Meet') RETURNING id", id).Scan(&meet)
	}
	if e == nil {
		_, e = tx.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=?", meet, id)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e == nil {
		e = a.loginSession(w, r, id)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/", 303)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	password := r.PostForm.Get("password")
	if len(password) > 256 {
		a.fail(w, r, 400, "Invalid credentials")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	defer func() { <-a.authSlots }()
	u, e := scanUser(a.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users WHERE email=?", strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))))
	hash := u.Password
	if e != nil {
		hash = "00000000000000000000000000000000:0000000000000000000000000000000000000000000000000000000000000000"
	}
	match := passwordMatches(hash, password)
	if e != nil || !match {
		a.render(w, r, "auth", Page{Title: "Sign in", Admin: true, RegistrationOpen: a.cfg.RegistrationOpen, Error: "Email or password is incorrect."}, 401)
		return
	}
	if e = a.loginSession(w, r, u.ID); e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/", 303)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("slot_session")
	if _, e := a.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token=?", hashToken(c.Value)); e != nil {
		a.internal(w, r, e)
		return
	}
	a.cookie(w, "slot_session", "", -1, true)
	http.Redirect(w, r, "/login", 303)
}

func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	old, newPass := r.PostForm.Get("old_password"), r.PostForm.Get("new_password")
	if len(old) > 256 || len(newPass) < 12 || len(newPass) > 256 {
		a.fail(w, r, 400, "Password must be 12–256 characters.")
		return
	}
	if !a.authCapacity(w) {
		return
	}
	defer func() { <-a.authSlots }()
	if !passwordMatches(u.Password, old) {
		a.fail(w, r, 403, "Current password is incorrect.")
		return
	}
	hash := passwordHash(newPass)
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(r.Context(), "UPDATE users SET password=? WHERE id=?", hash, u.ID)
	if e == nil {
		_, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=?", u.ID)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e == nil {
		e = a.loginSession(w, r, u.ID)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=password", 303)
}
