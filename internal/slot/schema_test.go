package slot

import (
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

func TestUpgradeFromVersion4(t *testing.T) {
	a, _ := testApp(t)
	seedHost(t, a, "alex")
	for _, q := range []string{"ALTER TABLE locations DROP COLUMN position", "PRAGMA user_version=4"} {
		if _, e := a.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	a.db.Close()
	b, e := New(a.cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer b.db.Close()
	var version, n int
	b.db.QueryRow("PRAGMA user_version").Scan(&version)
	if e = b.db.QueryRow("SELECT count(*) FROM locations WHERE position=0").Scan(&n); e != nil || version != schemaVersion || n != 2 {
		t.Fatalf("version %d, rows %d, error %v", version, n, e)
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
