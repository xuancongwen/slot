package slot

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSchemaForeignKeys(t *testing.T) {
	a, _ := testApp(t)
	var enabled int
	if e := a.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); e != nil || enabled != 1 {
		t.Fatal("foreign keys disabled")
	}
}

// downgrades undoes each upgrade step, so a test can rebuild any older database.
var downgrades = map[int]string{
	4: "ALTER TABLE locations DROP COLUMN position",
	5: "ALTER TABLE bookings DROP COLUMN reason",
	6: "ALTER TABLE bookings DROP COLUMN checked",
	8: "ALTER TABLE bookings DROP COLUMN verified",
	// Restores the version 7 status list. schema.sql recreates the trigger and indexes.
	7: `ALTER TABLE meeting_types DROP COLUMN approval;
CREATE TABLE bookings_old (
 id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 calendar_id INTEGER NOT NULL REFERENCES calendars(id),
 guest_name TEXT NOT NULL, guest_email TEXT NOT NULL,
 start INTEGER NOT NULL, end INTEGER NOT NULL, block_start INTEGER NOT NULL, block_end INTEGER NOT NULL,
 title TEXT NOT NULL, location TEXT NOT NULL, meet INTEGER NOT NULL DEFAULT 0, timezone TEXT NOT NULL,
 guest_timezone TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 manage_token TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL CHECK(status IN ('pending','confirmed','cancel_pending','cancelled','failed')),
 created INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 checked INTEGER NOT NULL DEFAULT 0,
 CHECK(end > start), CHECK(block_end > block_start)
);
INSERT INTO bookings_old SELECT * FROM bookings;
DROP TABLE bookings;
ALTER TABLE bookings_old RENAME TO bookings;`,
}

func TestUpgrade(t *testing.T) {
	for from := 4; from < schemaVersion; from++ {
		t.Run(fmt.Sprintf("from version %d", from), func(t *testing.T) {
			a, _ := testApp(t)
			u := seedHost(t, a, "alex")
			// A booking from before the upgrade must survive the bookings rebuild.
			if e := a.reserve(context.Background(), bookingFor(u, tomorrow().AddDate(0, 0, 1))); e != nil {
				t.Fatal(e)
			}
			for v := schemaVersion - 1; v >= from; v-- {
				if _, e := a.db.Exec(downgrades[v]); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := a.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", from)); e != nil {
				t.Fatal(e)
			}
			a.db.Close()
			b, e := New(a.cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer b.db.Close()
			var version int
			b.db.QueryRow("PRAGMA user_version").Scan(&version)
			if version != schemaVersion {
				t.Fatalf("version %d", version)
			}
			var n int
			if e = b.db.QueryRow("SELECT count(*) FROM bookings").Scan(&n); e != nil || n != 1 {
				t.Fatalf("%d bookings kept, error %v", n, e)
			}
			if ls, e := b.locations(context.Background(), u); e != nil || len(ls) != 2 {
				t.Fatalf("locations %v, error %v", ls, e)
			}
			booking := bookingFor(u, tomorrow())
			booking.Status = "requested"
			if e = b.reserve(context.Background(), booking); e != nil {
				t.Fatal(e)
			}
			if _, e = b.getBooking(context.Background(), booking.ManageToken); e != nil {
				t.Fatal(e)
			}
			if e = b.reserve(context.Background(), bookingFor(u, tomorrow())); e == nil {
				t.Fatal("overlap trigger missing after upgrade")
			}
		})
	}
}

func TestOldSchemaRefused(t *testing.T) {
	a, _ := testApp(t)
	if _, e := a.db.Exec("PRAGMA user_version=1"); e != nil {
		t.Fatal(e)
	}
	a.db.Close()
	if _, e := New(a.cfg); e == nil || !strings.Contains(e.Error(), "schema version") {
		t.Fatalf("old schema accepted: %v", e)
	}
}
