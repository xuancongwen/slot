package slot

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
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
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, p Page, status int) {
	p.CSRF = a.csrfToken(w, r, p.Admin)
	var buf bytes.Buffer
	if e := a.templates.ExecuteTemplate(&buf, name, p); e != nil {
		slog.Error("render", "template", name, "error", e)
		http.Error(w, "Could not render page", http.StatusInternalServerError)
		return
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
	m.HandleFunc("POST /password", a.authenticated(a.changePassword))
	return a.middleware(m, true)
}
