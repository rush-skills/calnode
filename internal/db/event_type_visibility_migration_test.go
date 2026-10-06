package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 00073 adds event_types.visibility with DEFAULT 'org', and that default is
// the point: every event type that existed before it becomes organisation-visible.
// This applies the schema up to 00072, inserts a row the old way, then migrates on and
// checks the row came through as 'org'.
func TestMigration00073_existingRowsBecomeOrgVisible(t *testing.T) {
	database, err := Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations", 72); err != nil {
		t.Fatalf("migrate to 72: %v", err)
	}

	// The column must not exist yet, or this test proves nothing.
	var visibility sql.NullString
	if err := database.QueryRow(`SELECT visibility FROM event_types LIMIT 1`).Scan(&visibility); err == nil {
		t.Fatal("event_types.visibility exists before migration 00073")
	}

	if _, err := database.Exec(`INSERT INTO users (id, email, name) VALUES ('u1', 'u1@example.com', 'One')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('et1', 'u1', 'old', 'Old', 30)`); err != nil {
		t.Fatalf("seed event type: %v", err)
	}

	if err := Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var got string
	if err := database.QueryRow(`SELECT visibility FROM event_types WHERE id = 'et1'`).Scan(&got); err != nil {
		t.Fatalf("read visibility: %v", err)
	}
	if got != "org" {
		t.Errorf("pre-existing row visibility = %q; want org", got)
	}

	// And the CHECK holds for new rows.
	if _, err := database.Exec(`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, visibility) VALUES ('et2', 'u1', 'new', 'New', 30, 'team')`); err == nil {
		t.Error("insert with visibility='team' succeeded; want CHECK violation")
	}
}
