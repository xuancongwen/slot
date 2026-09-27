// Package slot is a self-hosted Google Calendar booking service: one SQLite
// database, a public booking listener, and a separate admin listener.
package slot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // The scratch container image has no zoneinfo.

	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"
)

// timezones.txt lists UTC plus the canonical IANA zones ("Z" entries in tzdata.zi,
// excluding Etc/*), so the picker offers one name per region rather than every alias.
//
//go:embed web/templates/*.html web/static/* schema.sql timezones.txt
var assets embed.FS

// App holds the database, keys, templates, and Google client shared by both listeners.
type App struct {
	db         *sql.DB
	cfg        Config
	aead       cipher.AEAD
	signingKey []byte
	templates  *template.Template
	timezones  []string
	google     CalendarProvider
	oauth      *oauth2.Config
	http       *http.Client
	limit      *Limiter
	syncMu     sync.Mutex
	authSlots  chan struct{}
}

// New opens (creating if needed) the data directory, its encryption key, and database.
func New(c Config) (*App, error) {
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(c.DataDir, "secret.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// Never silently replace a lost key for an existing database.
		if _, e := os.Stat(filepath.Join(c.DataDir, "slot.db")); e == nil {
			return nil, errors.New("secret.key missing next to existing database; restore it from backup")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("secret.key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(c.DataDir, "slot.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // One short-lived SQL operation at a time; never hold a connection across Google calls.
	if err = migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	schema, _ := assets.ReadFile("schema.sql")
	if _, err = db.Exec(string(schema) + fmt.Sprintf("PRAGMA user_version=%d;", schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(filepath.Join(c.DataDir, "slot.db"), 0600); err != nil {
		db.Close()
		return nil, err
	}
	t, err := template.New("").Funcs(template.FuncMap{"clock": func(n int) string { return fmt.Sprintf("%02d:%02d", n/60, n%60) }, "hasDay": func(days string, d int) bool { return strings.Contains(days, fmt.Sprint(d)) }, "dateTime": func(t int64, tz string) string {
		loc, e := time.LoadLocation(tz)
		if e != nil {
			loc = time.UTC
		}
		return time.Unix(t, 0).In(loc).Format("Mon, Jan 2 · 15:04 MST")
	}, "guestZone": func(b Booking) string {
		if b.GuestTimezone != "" {
			return b.GuestTimezone
		}
		return b.Timezone
	}, "isLink": func(s string) bool {
		return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
	}, "lastIndex": func(ls []Location) int {
		return len(ls) - 1
	}, "hasMeet": func(ls []Location) bool {
		for _, l := range ls {
			if l.Kind == "meet" {
				return true
			}
		}
		return false
	}, "utcOffset": func(tz string) string {
		loc, e := time.LoadLocation(tz)
		if e != nil {
			return ""
		}
		return time.Now().In(loc).Format("UTC-07:00")
	}}).ParseFS(assets, "web/templates/*.html")
	if err != nil {
		db.Close()
		return nil, err
	}
	zones, _ := assets.ReadFile("timezones.txt")
	a := &App{db: db, cfg: c, aead: aead, templates: t, timezones: strings.Fields(string(zones)), http: &http.Client{Timeout: 15 * time.Second}, limit: newLimiter(), authSlots: make(chan struct{}, 4)}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("slot-ticket-signing-key-v1"))
	a.signingKey = mac.Sum(nil)
	a.oauth = &oauth2.Config{ClientID: c.GoogleClientID, ClientSecret: c.GoogleClientSecret, RedirectURL: c.AdminURL + "/oauth/callback", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token"}, Scopes: []string{"https://www.googleapis.com/auth/calendar.calendarlist.readonly", "https://www.googleapis.com/auth/calendar.events.freebusy", "https://www.googleapis.com/auth/calendar.events"}}
	a.google = &Google{app: a, baseURL: "https://www.googleapis.com/calendar/v3"}
	return a, nil
}

// Close releases the database.
func (a *App) Close() error { return a.db.Close() }

// Run serves both listeners and the calendar sync worker until ctx is cancelled,
// then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	public := server(a.cfg.PublicAddr, a.publicHandler())
	admin := server(a.cfg.AdminAddr, a.adminHandler())
	pl, err := net.Listen("tcp", a.cfg.PublicAddr)
	if err != nil {
		return fmt.Errorf("public listener: %w", err)
	}
	al, err := net.Listen("tcp", a.cfg.AdminAddr)
	if err != nil {
		pl.Close()
		return fmt.Errorf("admin listener: %w", err)
	}
	slog.Info("Slot ready", "public", a.cfg.PublicURL, "admin", a.cfg.AdminURL)
	var wg sync.WaitGroup
	for _, pair := range []struct {
		s *http.Server
		l net.Listener
	}{{public, pl}, {admin, al}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := pair.s.Serve(pair.l); e != nil && !errors.Is(e, http.ErrServerClosed) {
				slog.Error("server", "error", e)
				stop()
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.worker(ctx) }()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	public.Shutdown(shutdown)
	admin.Shutdown(shutdown)
	wg.Wait()
	return nil
}

func server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *App) seal(data []byte) []byte {
	n := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		panic(err)
	}
	return a.aead.Seal(n, n, data, nil)
}

func (a *App) open(data []byte) ([]byte, error) {
	n := a.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("invalid encrypted token")
	}
	return a.aead.Open(nil, data[:n], data[n:], nil)
}
