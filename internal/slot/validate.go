package slot

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
)

func validEmail(s string) bool {
	m, e := mail.ParseAddress(s)
	return e == nil && m.Address == s && len(s) <= 254 && !strings.ContainsAny(s, "\r\n")
}

func parseMinutes(s string) (int, error) {
	t, e := time.Parse("15:04", s)
	if e != nil {
		return 0, e
	}
	return t.Hour()*60 + t.Minute(), nil
}

// "Local" would silently follow the server's zone rather than the host's.
func validTimezone(tz string) bool {
	_, e := time.LoadLocation(tz)
	return tz != "" && tz != "Local" && e == nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

// slugify turns a name like "Coffee chat (30 min)" into "coffee-chat-30-min".
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	slug := strings.TrimRight(b.String()[:min(b.Len(), 40)], "-")
	if slug == "" {
		return "meeting"
	}
	return slug
}

const dateLayout = "2006-01-02"
