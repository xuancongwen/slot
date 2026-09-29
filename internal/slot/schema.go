package slot

import (
	"database/sql"
	"fmt"
)

// Bump schemaVersion whenever schema.sql changes shape, and add the step that brings
// the previous version's database up to it. Databases older than the first step
// predate migrations and must start fresh.
const schemaVersion = 10

func migrate(db *sql.DB) error {
	steps := map[int]string{
		4: "ALTER TABLE locations ADD COLUMN position INTEGER NOT NULL DEFAULT 0;",
		5: "ALTER TABLE bookings ADD COLUMN reason TEXT NOT NULL DEFAULT '';",
		6: "ALTER TABLE bookings ADD COLUMN checked INTEGER NOT NULL DEFAULT 0;",
		// SQLite cannot alter a CHECK constraint, so the new statuses need a rebuilt table.
		// Dropping the old table drops its indexes and trigger; schema.sql recreates them.
		7: `ALTER TABLE meeting_types ADD COLUMN approval INTEGER NOT NULL DEFAULT 0;
CREATE TABLE bookings_new (
 id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 calendar_id INTEGER NOT NULL REFERENCES calendars(id),
 guest_name TEXT NOT NULL, guest_email TEXT NOT NULL,
 start INTEGER NOT NULL, end INTEGER NOT NULL, block_start INTEGER NOT NULL, block_end INTEGER NOT NULL,
 title TEXT NOT NULL, location TEXT NOT NULL, meet INTEGER NOT NULL DEFAULT 0, timezone TEXT NOT NULL,
 guest_timezone TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 manage_token TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL CHECK(status IN ('requested','pending','confirmed','cancel_pending','cancelled','declined','failed')),
 created INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 checked INTEGER NOT NULL DEFAULT 0,
 CHECK(end > start), CHECK(block_end > block_start)
);
INSERT INTO bookings_new(` + bookingColumns + `,checked) SELECT ` + bookingColumns + `,checked FROM bookings;
DROP TABLE bookings;
ALTER TABLE bookings_new RENAME TO bookings;`,
		8: `ALTER TABLE meeting_types ADD COLUMN all_locations INTEGER NOT NULL DEFAULT 1;
ALTER TABLE meeting_types ADD COLUMN guest_location INTEGER NOT NULL DEFAULT 1;
CREATE TABLE meeting_type_locations (
 meeting_type_id INTEGER NOT NULL REFERENCES meeting_types(id) ON DELETE CASCADE,
 location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
 PRIMARY KEY(meeting_type_id,location_id)
);`,
		// Each day off becomes a one-day range.
		9: `CREATE TABLE blocks_new (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 first_day TEXT NOT NULL, last_day TEXT NOT NULL, CHECK(last_day >= first_day)
);
INSERT INTO blocks_new(id,user_id,first_day,last_day) SELECT id,user_id,day,day FROM blocks;
DROP TABLE blocks;
ALTER TABLE blocks_new RENAME TO blocks;`,
	}
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 || version == schemaVersion {
		return nil
	}
	if _, ok := steps[version]; !ok || version > schemaVersion {
		return fmt.Errorf("database schema version %d cannot be upgraded to this build's %d; move the data directory aside and start fresh", version, schemaVersion)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for ; version < schemaVersion; version++ {
		if _, err = tx.Exec(steps[version]); err != nil {
			return fmt.Errorf("upgrading database schema from version %d: %w", version, err)
		}
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d;", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
