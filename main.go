package main

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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"
)

// timezones.txt lists UTC plus the canonical IANA zones ("Z" entries in tzdata.zi,
// excluding Etc/*), so the picker offers one name per region rather than every alias.
//
//go:embed templates/*.html static/* schema.sql timezones.txt
var assets embed.FS

// Bump schemaVersion whenever schema.sql changes shape, and add the step that brings
// the previous version's database up to it. Databases older than the first step
// predate migrations and must start fresh.
const schemaVersion = 5

func migrate(db *sql.DB) error {
	steps := map[int]string{
		4: "ALTER TABLE locations ADD COLUMN position INTEGER NOT NULL DEFAULT 0;",
	}
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 || version == schemaVersion {
		return nil
	}
	if _, ok := steps[version]; !ok || version > schemaVersion {
		return fmt.Errorf("database schema version %d cannot be upgraded to this build's %d; move the data directory aside and start fresh", version, schemaVersion)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for ; version < schemaVersion; version++ {
		if _, err = tx.Exec(steps[version]); err != nil {
			return fmt.Errorf("upgrading database schema from version %d: %w", version, err)
		}
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d;", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

type Config struct {
	DataDir, PublicAddr, AdminAddr, PublicURL, AdminURL  string
	GoogleClientID, GoogleClientSecret, RegistrationCode string
	RegistrationOpen                                     bool
}

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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func config() (Config, error) {
	c := Config{DataDir: env("DATA_DIR", "data"), PublicAddr: env("PUBLIC_ADDR", ":8080"), AdminAddr: env("ADMIN_ADDR", "127.0.0.1:8081"), PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"), AdminURL: strings.TrimRight(env("ADMIN_URL", "http://localhost:8081"), "/"), GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"), GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"), RegistrationCode: os.Getenv("REGISTRATION_CODE"), RegistrationOpen: env("REGISTRATION_OPEN", "true") == "true"}
	for _, raw := range []string{c.PublicURL, c.AdminURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("PUBLIC_URL and ADMIN_URL must be absolute http(s) origins without a path")
		}
	}
	if c.PublicURL == c.AdminURL {
		return c, errors.New("public and admin origins must differ")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return c, errors.New("set both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET")
	}
	return c, nil
}

func newApp(c Config) (*App, error) {
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
	}}).ParseFS(assets, "templates/*.html")
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

func main() {
	c, err := config()
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(1)
	}
	a, err := newApp(c)
	if err != nil {
		slog.Error("startup", "error", err)
		os.Exit(1)
	}
	defer a.db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	public := server(c.PublicAddr, a.publicHandler())
	admin := server(c.AdminAddr, a.adminHandler())
	pl, err := net.Listen("tcp", c.PublicAddr)
	if err != nil {
		slog.Error("public listener", "error", err)
		os.Exit(1)
	}
	al, err := net.Listen("tcp", c.AdminAddr)
	if err != nil {
		pl.Close()
		slog.Error("admin listener", "error", err)
		os.Exit(1)
	}
	slog.Info("Slot ready", "public", c.PublicURL, "admin", c.AdminURL)
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
