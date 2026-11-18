package db

// Fork-only migrations (rush-skills/calnode).
//
// Upstream Calnode numbers its migrations in internal/db/migrations, and this fork
// keeps that folder byte-identical to upstream's so that merging upstream never
// collides on a migration number again. The fork's own schema changes live in
// internal/db/migrations_fork, numbered from 00001, and goose records them in their
// own table, goose_db_version_fork. They run after every upstream migration, on every
// boot, so a fork migration may rely on any upstream column but must stay additive
// (no table rebuilds): on a fresh install it runs after upstream changes that, on an
// older database, came later.
//
// When a fork migration is accepted upstream, keep the fork file (databases have
// already applied it) and make the upstream copy a no-op guard, or drop the fork
// file and add a remap here; see docs/FORK.md.

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations_fork/*.sql
var forkMigrations embed.FS

const (
	forkMigrationsDir = "migrations_fork"
	forkVersionTable  = "goose_db_version_fork"
	upstreamTable     = "goose_db_version" // goose's default; restored after the fork run
)

// Before the split, the fork's migrations shared upstream's numbering, and a database
// migrated by the fork holds them in goose_db_version at versions 68..82. Two of
// those (79, 80) are upstream's own 00068/00069 under the fork's old numbers. These
// maps say where each one lives now.
var (
	legacyToFork = map[int64]int64{
		68: 1, 69: 2, 70: 3, 71: 4, 72: 5, 73: 6, 74: 7, 75: 8, 76: 9, 77: 10, 78: 11,
		81: 12, 82: 13,
	}
	legacyToUpstream = map[int64]int64{79: 68, 80: 69}
)

// prepareForkMigrations rewrites a pre-split fork database's goose bookkeeping, once,
// in one transaction: the fork's rows move to goose_db_version_fork under their new
// numbers, and the two upstream migrations get back their upstream numbers. Nothing
// in the schema changes. Without this, goose would read the fork's 70..72 as
// upstream's 70..72 and skip upstream's real ones.
//
// It acts only on a database that has goose_db_version, has no fork table yet, and
// has the fork-only live_events table: a fresh install, a pure upstream database and
// an already converted one are left alone.
func prepareForkMigrations(db *sql.DB) error {
	if !tableExists(db, upstreamTable) || tableExists(db, forkVersionTable) || !tableExists(db, "live_events") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("fork migrations: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.Query(`SELECT version_id, tstamp FROM goose_db_version
		WHERE is_applied = 1 AND version_id BETWEEN 68 AND 82 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("fork migrations: read legacy versions: %w", err)
	}
	type row struct {
		v  int64
		ts sql.NullString
	}
	var legacy []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.v, &r.ts); err != nil {
			rows.Close() // #nosec G104 -- returning the scan error
			return fmt.Errorf("fork migrations: scan legacy version: %w", err)
		}
		legacy = append(legacy, r)
	}
	rows.Close() // #nosec G104 -- drained

	// The same table goose's SQLite dialect creates, so goose takes it over as its own.
	if _, err := tx.Exec(`CREATE TABLE ` + forkVersionTable + ` (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("fork migrations: create %s: %w", forkVersionTable, err)
	}
	if _, err := tx.Exec(`INSERT INTO ` + forkVersionTable + ` (version_id, is_applied) VALUES (0, 1)`); err != nil {
		return fmt.Errorf("fork migrations: seed %s: %w", forkVersionTable, err)
	}
	var moved, restored []int64
	for _, r := range legacy {
		if fv, ok := legacyToFork[r.v]; ok {
			if _, err := tx.Exec(`INSERT INTO `+forkVersionTable+` (version_id, is_applied, tstamp) VALUES (?, 1, COALESCE(?, datetime('now')))`, fv, r.ts); err != nil {
				return fmt.Errorf("fork migrations: record fork %d: %w", fv, err)
			}
			moved = append(moved, r.v)
		}
	}
	if _, err := tx.Exec(`DELETE FROM goose_db_version WHERE version_id BETWEEN 68 AND 82`); err != nil {
		return fmt.Errorf("fork migrations: clear legacy versions: %w", err)
	}
	for _, r := range legacy {
		if uv, ok := legacyToUpstream[r.v]; ok {
			if _, err := tx.Exec(`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (?, 1, COALESCE(?, datetime('now')))`, uv, r.ts); err != nil {
				return fmt.Errorf("fork migrations: record upstream %d: %w", uv, err)
			}
			restored = append(restored, uv)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fork migrations: commit: %w", err)
	}
	slog.Info("fork migrations: moved pre-split fork versions to their own table",
		"legacy_versions", moved, "fork_table", forkVersionTable, "upstream_versions_restored", restored)
	return nil
}

// migrateFork applies the fork's own migrations, after upstream's, in their own goose
// table, then the fork's boot-time schema fix-ups.
func migrateFork(db *sql.DB) error {
	goose.SetBaseFS(forkMigrations)
	goose.SetTableName(forkVersionTable)
	defer func() {
		goose.SetBaseFS(migrations)
		goose.SetTableName(upstreamTable)
	}()
	if err := goose.Up(db, forkMigrationsDir); err != nil {
		return fmt.Errorf("run fork migrations: %w", err)
	}
	return ensureEmailNoCaseIndex(db)
}

// ensureEmailNoCaseIndex adds the case-insensitive unique index on users.email (the
// pair of fork migration 00011). SQL cannot create it conditionally, and a database
// holding two accounts whose addresses differ only by case must keep booting: the
// index is created only once no such pair exists, otherwise each colliding address is
// logged for the operator to merge or remove, and boot carries on without it.
func ensureEmailNoCaseIndex(db *sql.DB) error {
	rows, err := db.Query(`SELECT lower(email), COUNT(*) FROM users GROUP BY lower(email) HAVING COUNT(*) > 1`)
	if err != nil {
		return fmt.Errorf("check email case collisions: %w", err)
	}
	var collisions []string
	for rows.Next() {
		var e string
		var n int
		if err := rows.Scan(&e, &n); err == nil {
			collisions = append(collisions, fmt.Sprintf("%s (%d accounts)", e, n))
		}
	}
	rows.Close() // #nosec G104 -- fully consumed above
	if len(collisions) > 0 {
		slog.Warn("users.email: accounts differing only by letter case exist; sign-in matches case-insensitively, "+
			"so merge or remove the duplicates (the case-insensitive unique index is not created until then)",
			"emails", collisions)
		return nil
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_nocase ON users (email COLLATE NOCASE)`); err != nil {
		return fmt.Errorf("create idx_users_email_nocase: %w", err)
	}
	return nil
}

var (
	forkTargetOnce sync.Once
	forkTarget     int64
	forkTargetErr  error
)

// ForkTargetVersion is the highest fork migration embedded in the binary.
func ForkTargetVersion() (int64, error) {
	forkTargetOnce.Do(func() {
		entries, err := fs.ReadDir(forkMigrations, forkMigrationsDir)
		if err != nil {
			forkTargetErr = fmt.Errorf("read embedded fork migrations: %w", err)
			return
		}
		for _, e := range entries {
			numPart, _, _ := strings.Cut(e.Name(), "_")
			if v, err := strconv.ParseInt(numPart, 10, 64); err == nil && v > forkTarget {
				forkTarget = v
			}
		}
	})
	return forkTarget, forkTargetErr
}

// ForkAppliedVersion is the highest fork migration applied to db.
func ForkAppliedVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM `+forkVersionTable+` WHERE is_applied = 1`).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// forkSchemaReady extends SchemaReady to the fork's migrations, so /readyz does not
// route traffic to a binary whose fork schema is still behind.
func forkSchemaReady(ctx context.Context, db *sql.DB) (bool, error) {
	target, err := ForkTargetVersion()
	if err != nil {
		return false, err
	}
	applied, err := ForkAppliedVersion(ctx, db)
	if err != nil {
		return false, err
	}
	return applied >= target, nil
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}
