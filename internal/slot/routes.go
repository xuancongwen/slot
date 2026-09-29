package slot

import (
	"bytes"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

type Page struct {
	Title, CSRF, Error, Notice                          string
	Admin, Register, RegistrationOpen, GoogleConfigured bool
	User                                                User
	Calendars                                           []Calendar
	Accounts                                            []Account
	Bookings                                            []Booking
	Blocks                                              []string
	Days                                                []DayOption
	MeetingTypes                                        []MeetingType
	Locations                                           []Location
	MeetingType                                         MeetingType
	Timezones                                           []string
	Slots                                               []Slot
	Weeks                                               [][]CalendarDay
	Date, BookingURL, SingleHost                        string
	Month, MonthLabel, PrevMonth, NextMonth             string
	GuestTimezone, SelectedLabel                        string
	DetectTimezone                                      bool
	Booking                                             Booking
	Start                                               int64
	SlotLabel, Ticket                                   string

	// ViewURL is where the booking calendar is shown: /b/host/type, or /b/host
	// or / when that is the only type. Date and time links stay on it.
	ViewURL string

	// Analytics loads the page view tracker when one is configured; render then
	// fills Tracker and widens the page's security policy to allow it.
	Analytics bool
	Tracker   template.HTML
}

// trackerTag is the script element for the configured page view tracker.
func (c Config) trackerTag() template.HTML {
	var b strings.Builder
	b.WriteString(`<script defer src="` + template.HTMLEscapeString(c.AnalyticsScript) + `"`)
	for _, attr := range c.AnalyticsAttrs {
		b.WriteString(" " + template.HTMLEscapeString(attr[0]) + `="` + template.HTMLEscapeString(attr[1]) + `"`)
	}
	b.WriteString("></script>")
	return template.HTML(b.String())
}

// trackerCSP lets the tracker's origin serve its script and receive page views.
func (c Config) trackerCSP() string {
	u, _ := url.Parse(c.AnalyticsScript)
	origin := u.Scheme + "://" + u.Host
	return "default-src 'none'; style-src 'self'; img-src 'self'; script-src 'self' " + origin + "; connect-src " + origin + "; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, p Page, status int) {
	p.CSRF = a.csrfToken(w, r, p.Admin)
	p.Analytics = p.Analytics && a.cfg.AnalyticsScript != ""
	if p.Analytics {
		p.Tracker = a.cfg.trackerTag()
	}
	var buf bytes.Buffer
	if e := a.templates.ExecuteTemplate(&buf, name, p); e != nil {
		slog.Error("render", "template", name, "error", e)
		http.Error(w, "Could not render page", http.StatusInternalServerError)
		return
	}
	if p.Analytics {
		w.Header().Set("Content-Security-Policy", a.cfg.trackerCSP())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	a.render(w, r, "error", Page{Title: "Something needs attention", Error: message}, status)
}

func (a *App) internal(w http.ResponseWriter, r *http.Request, e error) {
	slog.Error("request", "path", r.URL.Path, "error", e)
	a.fail(w, r, http.StatusInternalServerError, "Something went wrong. Please try again.")
}

func (a *App) commonRoutes(m *http.ServeMux) {
	sub, _ := fs.Sub(assets, "web/static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := a.db.PingContext(r.Context()); e != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
}

// adminURL is the path of an admin page, such as adminURL("/login").
func (a *App) adminURL(path string) string { return a.adminPath + path }

// siteHandler is the public listener: booking pages, plus the admin under /admin
// unless it is hosted separately. Each keeps its own middleware and CSRF cookie.
func (a *App) siteHandler() http.Handler {
	if a.adminPath == "" {
		return a.publicHandler()
	}
	m := http.NewServeMux()
	m.Handle("/", a.publicHandler())
	m.Handle(a.adminPath+"/", http.StripPrefix(a.adminPath, a.adminHandler()))
	m.Handle("GET "+a.adminPath, http.RedirectHandler(a.adminPath+"/", http.StatusMovedPermanently))
	return m
}

func (a *App) publicHandler() http.Handler {
	m := http.NewServeMux()
	a.commonRoutes(m)
	m.HandleFunc("GET /{$}", a.home)
	m.HandleFunc("GET /b/{slug}", a.hostPage)
	m.HandleFunc("GET /b/{slug}/{type}", a.publicPage)
	m.HandleFunc("POST /b/{slug}/{type}", a.book)
	m.HandleFunc("GET /manage/{token}", a.manage)
	m.HandleFunc("POST /manage/{token}/cancel", a.cancelPublic)
	m.HandleFunc("GET /manage/{token}/event.ics", a.ics)
	return a.middleware(m, false)
}

func (a *App) adminHandler() http.Handler {
	m := http.NewServeMux()
	a.commonRoutes(m)
	m.HandleFunc("GET /login", a.authPage)
	m.HandleFunc("POST /login", a.login)
	m.HandleFunc("GET /register", a.authPage)
	m.HandleFunc("POST /register", a.register)
	m.HandleFunc("GET /{$}", a.authenticated(a.dashboard))
	m.HandleFunc("POST /logout", a.authenticated(a.logout))
	m.HandleFunc("POST /settings", a.authenticated(a.settings))
	m.HandleFunc("GET /types/new", a.authenticated(a.meetingTypePage))
	m.HandleFunc("POST /types", a.authenticated(a.saveMeetingType))
	m.HandleFunc("GET /types/{id}", a.authenticated(a.meetingTypePage))
	m.HandleFunc("POST /types/{id}", a.authenticated(a.saveMeetingType))
	m.HandleFunc("POST /types/{id}/delete", a.authenticated(a.deleteMeetingType))
	m.HandleFunc("POST /types/{id}/active", a.authenticated(a.setMeetingTypeActive))
	m.HandleFunc("POST /locations", a.authenticated(a.addLocation))
	m.HandleFunc("POST /locations/{id}/default", a.authenticated(a.defaultLocation))
	m.HandleFunc("POST /locations/{id}/delete", a.authenticated(a.deleteLocation))
	m.HandleFunc("POST /locations/{id}/move", a.authenticated(a.moveLocation))
	m.HandleFunc("POST /calendars", a.authenticated(a.saveCalendarSettings))
	m.HandleFunc("POST /blocks", a.authenticated(a.blockDay))
	m.HandleFunc("POST /blocks/delete", a.authenticated(a.unblockDay))
	m.HandleFunc("POST /oauth/google", a.authenticated(a.oauthStart))
	m.HandleFunc("GET /oauth/callback", a.authenticated(a.oauthCallback))
	m.HandleFunc("POST /calendars/refresh", a.authenticated(a.refreshCalendars))
	m.HandleFunc("POST /bookings/{id}/cancel", a.authenticated(a.cancelAdmin))
	m.HandleFunc("POST /bookings/{id}/retry", a.authenticated(a.retryAdmin))
	m.HandleFunc("POST /bookings/{id}/approve", a.authenticated(a.approveAdmin))
	m.HandleFunc("POST /bookings/{id}/decline", a.authenticated(a.declineAdmin))
	m.HandleFunc("POST /password", a.authenticated(a.changePassword))
	return a.middleware(m, true)
}
