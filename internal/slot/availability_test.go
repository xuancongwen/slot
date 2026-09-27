package slot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAvailabilityRules(t *testing.T) {
	u := MeetingType{Timezone: "UTC", Days: "12345", StartMin: 540, EndMin: 660, Duration: 30, Buffer: 10, Notice: 60, Horizon: 30}
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	now := day.Add(8 * time.Hour)
	busy := []Span{{day.Add(9*time.Hour + 30*time.Minute), day.Add(10 * time.Hour)}}
	s := generateSlots(u, day, now, busy)
	if len(s) != 1 || s[0].Start != day.Add(10*time.Hour+30*time.Minute).Unix() {
		t.Fatalf("buffer/notice: %+v", s)
	}
	if got := generateSlots(u, day.AddDate(0, 0, 5), now, nil); len(got) != 0 {
		t.Fatal("Saturday should be closed")
	}
	if got := generateSlots(u, day.AddDate(0, 0, 40), now, nil); len(got) != 0 {
		t.Fatal("outside horizon")
	}
}

func TestDST(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	u := MeetingType{Timezone: loc.String(), Days: "0", StartMin: 120, EndMin: 180, Duration: 30, Horizon: 30}
	spring := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	if s := generateSlots(u, spring, spring.AddDate(0, 0, -1), nil); len(s) != 0 {
		t.Fatalf("spring gap exposed nonexistent times: %+v", s)
	}
	fall := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	u.StartMin = 60
	u.EndMin = 180
	s := generateSlots(u, fall, fall.AddDate(0, 0, -1), nil)
	if len(s) != 6 {
		t.Fatalf("fall repeated hour: got %d: %+v", len(s), s)
	}
	seen := map[int64]bool{}
	for _, x := range s {
		if seen[x.Start] {
			t.Fatal("duplicate instant")
		}
		seen[x.Start] = true
	}
	if s[0].Label == s[2].Label {
		t.Fatal("ambiguous repeated hour labels")
	}
}

func TestFailClosedAndDaysOff(t *testing.T) {
	a, f := testApp(t)
	u := seedHost(t, a, "alex")
	mt := chatType(t, a, u)
	day := tomorrow()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	f.busyErr = errors.New("Google unavailable")
	if _, e := a.availability(context.Background(), u, mt, day, day.AddDate(0, 0, 1), time.Now()); e == nil {
		t.Fatal("availability allowed on Google error")
	}
	f.busyErr = nil
	_, e := a.db.Exec("INSERT INTO blocks(user_id,day) VALUES(?,?)", u.ID, day.Format("2006-01-02"))
	if e != nil {
		t.Fatal(e)
	}
	s, e := a.availability(context.Background(), u, mt, day, day.AddDate(0, 0, 1), time.Now())
	if e != nil || len(s) > 0 {
		t.Fatal("day off ignored")
	}
}

// A guest's day in Singapore spans two of a UTC meeting type's days; only slots
// starting inside the guest's day belong to it.
func TestAvailabilitySpansGuestDay(t *testing.T) {
	a, _ := testApp(t)
	u := seedHost(t, a, "alex")
	a.db.Exec("UPDATE meeting_types SET start_min=540,end_min=660 WHERE user_id=?", u.ID)
	mt := chatType(t, a, u)
	sgt := mustLoad(t, "Asia/Singapore")
	d := time.Now().In(sgt).AddDate(0, 0, 3)
	guestDay := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, sgt)
	slots, e := a.availability(context.Background(), u, mt, guestDay, guestDay.AddDate(0, 0, 1), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	var got []string
	for _, s := range slots {
		got = append(got, time.Unix(s.Start, 0).UTC().Format("Jan 2 15:04"))
	}
	hostDay := guestDay.Format("Jan 2") // 00:00–24:00 SGT covers 09:00–11:00 UTC of the same date.
	if want := []string{hostDay + " 09:00", hostDay + " 09:30", hostDay + " 10:00", hostDay + " 10:30"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("slots %v, want %v", got, want)
	}
	a.db.Exec("INSERT INTO blocks(user_id,day) VALUES(?,?)", u.ID, guestDay.Format("2006-01-02"))
	if slots, _ = a.availability(context.Background(), u, mt, guestDay, guestDay.AddDate(0, 0, 1), time.Now()); len(slots) != 0 {
		t.Fatal("day off in the meeting type's timezone ignored")
	}
}

func BenchmarkSlotGeneration(b *testing.B) {
	u := MeetingType{Timezone: "America/Los_Angeles", Days: "0123456", StartMin: 540, EndMin: 1020, Duration: 30, Horizon: 30}
	loc, _ := time.LoadLocation(u.Timezone)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, loc)
	b.ReportAllocs()
	for b.Loop() {
		generateSlots(u, day, day.AddDate(0, 0, -1), nil)
	}
}
