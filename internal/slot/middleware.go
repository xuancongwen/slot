package slot

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (a *App) middleware(h http.Handler, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Not no-referrer: that makes browsers send "Origin: null" on same-origin form
		// POSTs, which the origin check below rejects. same-origin still leaks nothing
		// (such as manage tokens in URLs) to other sites.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		if admin {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self' https://accounts.google.com; frame-ancestors 'none'; base-uri 'none'")
		}
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if !a.limit.Allow(r, admin) {
			http.Error(w, "Too many requests. Please try again in a minute.", http.StatusTooManyRequests)
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
				http.Error(w, "Origin rejected", http.StatusForbidden)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "Cross-site request rejected", http.StatusForbidden)
				return
			}
			if e := r.ParseForm(); e != nil {
				http.Error(w, "Invalid form", http.StatusBadRequest)
				return
			}
			c, e := r.Cookie(name)
			if e != nil || len(c.Value) != 64 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
				http.Error(w, "Form expired. Reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) cookie(w http.ResponseWriter, name, value string, ttl int, admin bool) {
	origin := a.cfg.PublicURL
	if admin {
		origin = a.cfg.AdminURL
	}
	path := "/"
	if admin && a.adminPath != "" {
		path = a.adminPath // Keep the admin session off the booking pages.
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: strings.HasPrefix(origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: ttl})
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
