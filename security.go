package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
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
func (a *App) cookie(w http.ResponseWriter, name, value string, ttl int, admin bool) {
	origin := a.cfg.PublicURL
	if admin {
		origin = a.cfg.AdminURL
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: ttl})
}
func (a *App) csrfToken(w http.ResponseWriter, r *http.Request, admin bool) string {
	name := "slot_public_csrf"
	if admin {
		name = "slot_admin_csrf"
	}
	if c, e := r.Cookie(name); e == nil && len(c.Value) == 64 {
		return c.Value
	}
	token := randomHex(32)
	a.cookie(w, name, token, 86400, admin)
	return token
}
func (a *App) middleware(h http.Handler, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if admin {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self'; form-action 'self' https://accounts.google.com; frame-ancestors 'none'; base-uri 'none'")
		}
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if !a.limit.Allow(r, admin) {
			http.Error(w, "Too many requests. Please try again in a minute.", 429)
			return
		}
		if r.Method == http.MethodPost {
			origin := a.cfg.PublicURL
			name := "slot_public_csrf"
			if admin {
				origin = a.cfg.AdminURL
				name = "slot_admin_csrf"
			}
			if got := r.Header.Get("Origin"); got != "" && got != origin {
				http.Error(w, "Origin rejected", 403)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "Cross-site request rejected", 403)
				return
			}
			if e := r.ParseForm(); e != nil {
				http.Error(w, "Invalid form", 400)
				return
			}
			c, e := r.Cookie(name)
			if e != nil || len(c.Value) != 64 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
				http.Error(w, "Form expired. Reload the page and try again.", 403)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
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

type bucket struct {
	count int
	until time.Time
}
type Limiter struct {
	mu        sync.Mutex
	entries   map[string]bucket
	lastClean time.Time
}

func newLimiter() *Limiter { return &Limiter{entries: make(map[string]bucket)} }
func (l *Limiter) Allow(r *http.Request, admin bool) bool {
	if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
		return true
	}
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	limit := 120
	kind := "read"
	if r.Method == "POST" {
		kind = "write"
		limit = 20
	}
	if r.URL.Path == "/login" || r.URL.Path == "/register" {
		kind = "auth"
		limit = 15
	}
	key := fmt.Sprint(admin) + kind + host
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastClean) > time.Minute {
		for k, b := range l.entries {
			if now.After(b.until) {
				delete(l.entries, k)
			}
		}
		l.lastClean = now
	}
	b := l.entries[key]
	if now.After(b.until) {
		if len(l.entries) >= 10000 {
			return false
		}
		b = bucket{until: now.Add(time.Minute)}
	}
	b.count++
	l.entries[key] = b
	return b.count <= limit
}
