package slot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingKeyDoesNotReinitialize(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "slot.db"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := New(Config{DataDir: dir}); e == nil {
		t.Fatal("silently recreated encryption key")
	}
}

func TestTimezoneListLoads(t *testing.T) {
	a, _ := testApp(t)
	if len(a.timezones) < 300 || a.timezones[0] != "UTC" {
		t.Fatalf("timezone list: %d entries", len(a.timezones))
	}
	for _, tz := range a.timezones {
		if !validTimezone(tz) {
			t.Errorf("listed timezone does not load: %s", tz)
		}
	}
}
