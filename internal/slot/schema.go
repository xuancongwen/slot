package slot

import (
	"database/sql"
	"fmt"
)

// Bump schemaVersion whenever schema.sql changes shape, and add the step that brings
// the previous version's database up to it. Databases older than the first step
// predate migrations and must start fresh.
const schemaVersion = 6

func migrate(db *sql.DB) error {
	steps := map[int]string{
		4: "ALTER TABLE locations ADD COLUMN position INTEGER NOT NULL DEFAULT 0;",
		5: "ALTER TABLE bookings ADD COLUMN reason TEXT NOT NULL DEFAULT '';",
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
