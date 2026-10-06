package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

type testDB struct{ *sql.DB }

// openTo opens an in-memory database migrated up to version v.
func openTo(t *testing.T, v int64) *testDB {
	t.Helper()
	database, err := Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations", v); err != nil {
		t.Fatalf("migrate to %d: %v", v, err)
	}
	return &testDB{database}
}

// Migration 00078 lowercases stored addresses where that cannot collide, and
// ensureEmailNoCaseIndex adds the NOCASE unique index only once no pair of accounts
// differs by case alone - a database holding such a pair still boots, without the index.
func TestMigrate_emailCaseFolding(t *testing.T) {
	open := func(t *testing.T, upToVersion int64, seed []string) *testDB {
		t.Helper()
		d := openTo(t, upToVersion)
		for _, q := range seed {
			if _, err := d.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	hasIndex := func(d *testDB) bool {
		var n int
		d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_users_email_nocase'`).Scan(&n) //nolint:errcheck
		return n == 1
	}

	t.Run("lowercases and indexes a clean database", func(t *testing.T) {
		d := open(t, 77, []string{
			`INSERT INTO users (id,email,name,iana_timezone) VALUES ('a','Ada@Example.com','Ada','UTC')`,
			`INSERT INTO users (id,email,name,iana_timezone) VALUES ('b','bob@example.com','Bob','UTC')`,
		})
		if err := Migrate(d.DB); err != nil {
			t.Fatal(err)
		}
		var a string
		d.QueryRow(`SELECT email FROM users WHERE id = 'a'`).Scan(&a) //nolint:errcheck
		if a != "ada@example.com" || !hasIndex(d) {
			t.Errorf("email = %q index=%v; want lowercased and indexed", a, hasIndex(d))
		}
		if _, err := d.Exec(`INSERT INTO users (id,email,name,iana_timezone) VALUES ('c','BOB@example.com','Bob2','UTC')`); err == nil {
			t.Error("case variant accepted after the index was created")
		}
	})

	t.Run("a pair differing only by case keeps both rows and boots without the index", func(t *testing.T) {
		d := open(t, 77, []string{
			`INSERT INTO users (id,email,name,iana_timezone) VALUES ('a','Ada@Example.com','Ada','UTC')`,
			`INSERT INTO users (id,email,name,iana_timezone) VALUES ('a2','ada@example.com','Ada 2','UTC')`,
			`INSERT INTO users (id,email,name,iana_timezone) VALUES ('b','Bob@Example.com','Bob','UTC')`,
		})
		if err := Migrate(d.DB); err != nil {
			t.Fatalf("boot must not fail on a collision: %v", err)
		}
		var a, b string
		d.QueryRow(`SELECT email FROM users WHERE id = 'a'`).Scan(&a) //nolint:errcheck
		d.QueryRow(`SELECT email FROM users WHERE id = 'b'`).Scan(&b) //nolint:errcheck
		if a != "Ada@Example.com" || b != "bob@example.com" {
			t.Errorf("a=%q b=%q; want the colliding one untouched and the other lowercased", a, b)
		}
		if hasIndex(d) {
			t.Error("index created despite a remaining collision")
		}
		// Once the operator resolves it, the next boot adds the index.
		if _, err := d.Exec(`DELETE FROM users WHERE id = 'a2'`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(d.DB); err != nil {
			t.Fatal(err)
		}
		if !hasIndex(d) {
			t.Error("index not created after the collision was resolved")
		}
	})
}
