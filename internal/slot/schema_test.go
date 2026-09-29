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
}

func TestUpgrade(t *testing.T) {
	for from := 4; from < schemaVersion; from++ {
		t.Run(fmt.Sprintf("from version %d", from), func(t *testing.T) {
			a, _ := testApp(t)
			u := seedHost(t, a, "alex")
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
			if ls, e := b.locations(context.Background(), u); e != nil || len(ls) != 2 {
				t.Fatalf("locations %v, error %v", ls, e)
			}
			booking := bookingFor(u, tomorrow())
			if e = b.reserve(context.Background(), booking); e != nil {
				t.Fatal(e)
			}
			if _, e = b.getBooking(context.Background(), booking.ManageToken); e != nil {
				t.Fatal(e)
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
