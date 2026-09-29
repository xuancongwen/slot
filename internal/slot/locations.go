package slot

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func (a *App) addLocation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	kind, label, detail := "custom", strings.TrimSpace(r.PostForm.Get("label")), strings.TrimSpace(r.PostForm.Get("detail"))
	if r.PostForm.Get("kind") == "meet" {
		kind, label, detail = "meet", "Google Meet", ""
	} else if len(label) < 1 || len(label) > 60 || len(detail) > 500 {
		a.fail(w, r, http.StatusBadRequest, "Give the location a name up to 60 characters, and a link or address up to 500.")
		return
	}
	var id int64
	e := a.db.QueryRowContext(r.Context(), "INSERT INTO locations(user_id,kind,label,detail,position) VALUES(?,?,?,?,(SELECT coalesce(max(position),0)+1 FROM locations WHERE user_id=?)) RETURNING id", u.ID, kind, label, detail, u.ID).Scan(&id)
	if e == nil && !u.DefaultLocation.Valid {
		_, e = a.db.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=?", id, u.ID)
	}
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, a.adminURL("/?notice=saved#profile"), http.StatusSeeOther)
}

func (a *App) defaultLocation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	_, e := a.db.ExecContext(r.Context(), "UPDATE users SET default_location=? WHERE id=? AND EXISTS(SELECT 1 FROM locations WHERE id=? AND user_id=?)", r.PathValue("id"), u.ID, r.PathValue("id"), u.ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, a.adminURL("/?notice=saved#profile"), http.StatusSeeOther)
}

// moveLocation swaps a location with its neighbor, then renumbers the list so
// positions stay distinct even for rows that predate ordering.
func (a *App) moveLocation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	ls, e := a.locations(r.Context(), u)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	i := slices.IndexFunc(ls, func(l Location) bool { return strconv.FormatInt(l.ID, 10) == r.PathValue("id") })
	j := i + 1
	if r.PostForm.Get("direction") == "up" {
		j = i - 1
	}
	if i < 0 || j < 0 || j >= len(ls) {
		http.Redirect(w, r, a.adminURL("/#profile"), http.StatusSeeOther)
		return
	}
	ls[i], ls[j] = ls[j], ls[i]
	e = a.inTx(r.Context(), func(tx *sql.Tx) error {
		for position, l := range ls {
			if _, e := tx.ExecContext(r.Context(), "UPDATE locations SET position=? WHERE id=? AND user_id=?", position, l.ID, u.ID); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, a.adminURL("/#profile"), http.StatusSeeOther)
}

func (a *App) deleteLocation(w http.ResponseWriter, r *http.Request) {
	_, e := a.db.ExecContext(r.Context(), "DELETE FROM locations WHERE id=? AND user_id=?", r.PathValue("id"), currentUser(r).ID)
	if e != nil {
		a.internal(w, r, e)
		return
	}
	http.Redirect(w, r, a.adminURL("/?notice=saved#profile"), http.StatusSeeOther)
}

// chosenLocation resolves the guest's pick among the locations t offers. Their own text,
// where t allows it, wins over the selected option, so typing a location works without
// script to deselect the preselected default.
func (a *App) chosenLocation(r *http.Request, u User, t MeetingType) (text string, meet bool, err error) {
	text = strings.TrimSpace(r.PostForm.Get("custom_location"))
	if len(text) > 500 {
		return "", false, errors.New("custom location too long")
	}
	if text != "" && !t.AllowsGuestLocation() {
		return "", false, errors.New("meeting type takes no guest location")
	}
	choice := r.PostForm.Get("location")
	if text != "" {
		return text, false, nil
	}
	if choice == "" {
		if t.AllowsGuestLocation() {
			return "", false, nil
		}
		// Nothing to choose from is fine; skipping an offered choice is not.
		var offered bool
		if e := a.db.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM meeting_type_locations WHERE meeting_type_id=?)", t.ID).Scan(&offered); e != nil {
			return "", false, fmt.Errorf("checking offered locations: %w", e)
		}
		if offered {
			return "", false, errors.New("no location chosen")
		}
		return "", false, nil
	}
	var l Location
	e := a.db.QueryRowContext(r.Context(), "SELECT kind,label,detail FROM locations WHERE id=? AND user_id=? AND (? OR id IN (SELECT location_id FROM meeting_type_locations WHERE meeting_type_id=?))", choice, u.ID, t.AllLocations, t.ID).Scan(&l.Kind, &l.Label, &l.Detail)
	if e != nil {
		return "", false, fmt.Errorf("location %q: %w", choice, e)
	}
	if l.Kind == "meet" {
		// The link does not exist until Google creates the event.
		return "", true, nil
	}
	return l.Text(), false, nil
}
