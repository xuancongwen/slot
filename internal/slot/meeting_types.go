package slot

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type DayOption struct {
	Number int
	Name   string
}

var days = []DayOption{{1, "Mon"}, {2, "Tue"}, {3, "Wed"}, {4, "Thu"}, {5, "Fri"}, {6, "Sat"}, {0, "Sun"}}

// freeSlug appends -2, -3, … to t's generated slug until no other meeting type of u uses it.
func (a *App) freeSlug(r *http.Request, u User, t MeetingType) (string, error) {
	taken, e := queryAll(r.Context(), a.db, scanString, "SELECT slug FROM meeting_types WHERE user_id=? AND id!=?", u.ID, t.ID)
	if e != nil {
		return "", e
	}
	slug := t.Slug
	for n := 2; slices.Contains(taken, slug); n++ {
		suffix := fmt.Sprintf("-%d", n)
		slug = strings.TrimRight(t.Slug[:min(len(t.Slug), 40-len(suffix))], "-") + suffix
	}
	return slug, nil
}

// ownedMeetingType returns the zero MeetingType with no error for /types/new.
func (a *App) ownedMeetingType(r *http.Request) (MeetingType, error) {
	if r.PathValue("id") == "" {
		return MeetingType{Days: "12345", StartMin: 540, EndMin: 1020, Duration: 30, Notice: 120, Horizon: 30, Active: true}, nil
	}
	return scanMeetingType(a.db.QueryRowContext(r.Context(), "SELECT "+meetingTypeColumns+" FROM meeting_types WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID))
}

func (a *App) meetingTypePage(w http.ResponseWriter, r *http.Request) {
	t, e := a.ownedMeetingType(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	title := "New meeting type"
	if t.ID != 0 {
		title = t.Name
	}
	a.render(w, r, "meeting_type", Page{Title: title, Admin: true, User: currentUser(r), MeetingType: t, Days: days, Timezones: a.timezones, BookingURL: a.cfg.PublicURL + "/b/" + currentUser(r).Slug}, http.StatusOK)
}

func (a *App) saveMeetingType(w http.ResponseWriter, r *http.Request) {
	t, e := a.ownedMeetingType(r)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	var e1, e2, e3, e4, e5, e6 error
	t.Name = strings.TrimSpace(f.Get("name"))
	t.Slug = strings.ToLower(strings.TrimSpace(f.Get("slug")))
	if t.Slug == "" {
		t.Slug = slugify(t.Name) // Validated below; made unique once the rest of the form passes.
	}
	t.Timezone = strings.TrimSpace(f.Get("timezone"))
	t.StartMin, e1 = parseMinutes(f.Get("start"))
	t.EndMin, e2 = parseMinutes(f.Get("end"))
	t.Duration, e3 = strconv.Atoi(f.Get("duration"))
	t.Buffer, e4 = strconv.Atoi(f.Get("buffer"))
	t.Notice, e5 = strconv.Atoi(f.Get("notice"))
	t.Horizon, e6 = strconv.Atoi(f.Get("horizon"))
	t.Active = f.Get("active") == "on"
	t.Days = ""
	for _, d := range days {
		for _, v := range f["days"] {
			if v == strconv.Itoa(d.Number) {
				t.Days += v
				break
			}
		}
	}
	if len(t.Name) < 1 || len(t.Name) > 100 || !slugPattern.MatchString(t.Slug) || !validTimezone(t.Timezone) || e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || t.StartMin >= t.EndMin || t.Duration < 5 || t.Duration > 240 || t.Duration > t.EndMin-t.StartMin || t.Buffer < 0 || t.Buffer > 120 || t.Notice < 0 || t.Notice > 43200 || t.Horizon < 1 || t.Horizon > 90 {
		a.fail(w, r, http.StatusBadRequest, "Check the name, URL, timezone, and hours. Length: 5–240 minutes, buffer: 0–120 minutes, notice: 0–43200 minutes, booking horizon: 1–90 days.")
		return
	}
	if t.Active && t.Days == "" {
		a.fail(w, r, http.StatusBadRequest, "Choose at least one available weekday before turning this meeting type on.")
		return
	}
	u := currentUser(r)
	if f.Get("slug") == "" {
		if t.Slug, e = a.freeSlug(r, u, t); e != nil {
			a.internal(w, r, e)
			return
		}
	}
	if t.ID == 0 {
		_, e = a.db.ExecContext(r.Context(), `INSERT INTO meeting_types(user_id,slug,name,timezone,days,start_min,end_min,duration,buffer,notice,horizon,active) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, u.ID, t.Slug, t.Name, t.Timezone, t.Days, t.StartMin, t.EndMin, t.Duration, t.Buffer, t.Notice, t.Horizon, t.Active)
	} else {
		_, e = a.db.ExecContext(r.Context(), `UPDATE meeting_types SET slug=?,name=?,timezone=?,days=?,start_min=?,end_min=?,duration=?,buffer=?,notice=?,horizon=?,active=? WHERE id=? AND user_id=?`, t.Slug, t.Name, t.Timezone, t.Days, t.StartMin, t.EndMin, t.Duration, t.Buffer, t.Notice, t.Horizon, t.Active, t.ID, u.ID)
	}
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			a.fail(w, r, http.StatusConflict, "You already have a meeting type at that URL.")
		} else {
			a.internal(w, r, e)
		}
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
}

func (a *App) deleteMeetingType(w http.ResponseWriter, r *http.Request) {
	// Bookings keep their own copy of title, time, and timezone, so history survives.
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM meeting_types WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, "/?notice=deleted", http.StatusSeeOther)
}
