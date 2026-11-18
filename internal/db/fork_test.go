package db

import (
	"testing"
)

// A database migrated by the fork before its migrations moved to their own table holds
// them in goose_db_version at 68..82, two of them (79, 80) being upstream's 68 and 69.
// Migrate must convert that bookkeeping once, apply upstream's real 70..72, re-run no
// fork migration, and keep the data.
func TestMigrate_convertsPreSplitForkDatabase(t *testing.T) {
	// A pre-split database: upstream up to 69, the 13 fork migrations that existed before
	// the split (later ones were only ever numbered in the fork table), and upstream's
	// 70..72 not yet run.
	pre := openUpstreamTo(t, 69)
	applyForkTo(t, pre, 13)
	// Rewrite the bookkeeping into the pre-split layout.
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pre.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`DELETE FROM goose_db_version WHERE version_id IN (68, 69)`)
	for _, v := range []int64{68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82} {
		mustExec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v)
	}
	mustExec(`DROP TABLE ` + forkVersionTable)
	mustExec(`INSERT INTO users (id, email, name) VALUES ('u1', 'u1@example.com', 'One')`)
	mustExec(`INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('et1', 'u1', 'demo', 'Demo', 30)`)

	if err := Migrate(pre.DB); err != nil {
		t.Fatalf("Migrate on a pre-split database: %v", err)
	}

	// Upstream 70..72 ran: their columns exist.
	for _, q := range []string{
		`SELECT display_order FROM event_types LIMIT 1`,
		`SELECT payment_ref, payment_provider FROM bookings LIMIT 1`,
	} {
		if _, err := pre.Exec(q); err != nil {
			t.Errorf("upstream migration did not run (%s): %v", q, err)
		}
	}
	// The fork's columns survived and nothing was lost.
	var vis string
	if err := pre.QueryRow(`SELECT visibility FROM event_types WHERE id = 'et1'`).Scan(&vis); err != nil || vis != "org" {
		t.Errorf("fork column visibility = %q (%v)", vis, err)
	}
	// Bookkeeping: upstream at its own target, the fork at its own, no legacy rows.
	up, _ := AppliedVersion(t.Context(), pre.DB)
	ut, _ := TargetVersion()
	fk, _ := ForkAppliedVersion(t.Context(), pre.DB)
	ft, _ := ForkTargetVersion()
	if up != ut || fk != ft {
		t.Errorf("applied upstream %d/%d, fork %d/%d", up, ut, fk, ft)
	}
	var stray int
	_ = pre.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id BETWEEN 73 AND 82`).Scan(&stray)
	if stray != 0 && ut < 73 {
		t.Errorf("%d legacy fork rows left in goose_db_version", stray)
	}
	if ok, err := SchemaReady(t.Context(), pre.DB); err != nil || !ok {
		t.Errorf("SchemaReady = %v, %v", ok, err)
	}
	// A second boot changes nothing.
	if err := Migrate(pre.DB); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// A fresh database ends with every upstream and fork migration recorded in its own table.
func TestMigrate_freshDatabaseRecordsBothTables(t *testing.T) {
	d, err := Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	if ok, err := SchemaReady(t.Context(), d); err != nil || !ok {
		t.Fatalf("SchemaReady = %v, %v", ok, err)
	}
	var forkRows int
	_ = d.QueryRow(`SELECT COUNT(*) FROM ` + forkVersionTable + ` WHERE version_id > 0`).Scan(&forkRows)
	ft, _ := ForkTargetVersion()
	if int64(forkRows) != ft {
		t.Errorf("fork rows = %d; want %d", forkRows, ft)
	}
}
