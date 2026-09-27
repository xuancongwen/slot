package slot

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for name, want := range map[string]string{
		"30 minute chat":           "30-minute-chat",
		"  Coffee chat (30 min)! ": "coffee-chat-30-min",
		"Café ☕":                   "caf",
		"☕☕":                       "meeting",
		strings.Repeat("ab ", 30):  "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-a", // Cut at the 40-character limit.
	} {
		if got := slugify(name); got != want || !slugPattern.MatchString(got) {
			t.Errorf("slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestTwoCharacterBookingSlug(t *testing.T) {
	for _, slug := range []string{"a", "al", "alex", "alex-morgan"} {
		if !slugPattern.MatchString(slug) {
			t.Errorf("valid slug rejected: %s", slug)
		}
	}
	for _, slug := range []string{"", "-al", "al-", "../al", "al/ex", strings.Repeat("a", 41)} {
		if slugPattern.MatchString(slug) {
			t.Errorf("invalid slug accepted: %s", slug)
		}
	}
}
