package handler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// fallbackEventTypeSlug is used by the sweep when neither the stored slug nor the name
// yields anything after slugify (e.g. a slug of "!!!" on an event type called "???").
const fallbackEventTypeSlug = "event-type"

// NormalizeEventTypeSlugs rewrites every stored event type slug that is not already in
// canonical slugify() form, so that /book/{slug} resolves for all of them. It runs once
// at boot, right after migrations.
//
// Why it exists: until the create path was fixed, a slug was stored exactly as typed,
// so "Intro Call" produced a booking link with a space in it that never resolved. The
// update path normalised, the create path did not, and the result was rows whose link
// was broken from the moment they were made. Those rows are not fixed by fixing the
// handler; they need rewriting.
//
// Safety: bookings reference event types by id, not slug, so nothing downstream moves.
// A link to a non-canonical slug was already broken, so rewriting it breaks nothing
// that worked. Canonical slugs are never touched.
//
// Collisions: the slugs are processed oldest-first, and a candidate already taken (by
// an untouched row or an earlier rewrite) gets "-2", "-3", … appended, matching the
// "-copy-N" convention the duplicate endpoint uses. Every change is logged with the
// id, the old slug and the new one, so an operator can tell people whose links changed.
func NormalizeEventTypeSlugs(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	rows, err := db.QueryContext(ctx, `SELECT id, slug, name FROM event_types ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("normalize event type slugs: list: %w", err)
	}
	defer rows.Close()

	type fix struct{ id, slug, name string }
	taken := map[string]bool{}
	var fixes []fix
	for rows.Next() {
		var f fix
		if err := rows.Scan(&f.id, &f.slug, &f.name); err != nil {
			return fmt.Errorf("normalize event type slugs: scan: %w", err)
		}
		if slugify(f.slug) == f.slug {
			taken[f.slug] = true
			continue
		}
		fixes = append(fixes, f)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("normalize event type slugs: rows: %w", err)
	}
	if len(fixes) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("normalize event type slugs: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, f := range fixes {
		base := slugify(f.slug)
		if base == "" {
			base = slugify(f.name)
		}
		if base == "" {
			base = fallbackEventTypeSlug
		}
		// A canonical candidate can never equal a non-canonical stored slug (one is a
		// fixed point of slugify, the other is not), so `taken` only needs the rows that
		// keep their slug plus the rewrites made so far.
		candidate := base
		for n := 2; taken[candidate]; n++ {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		taken[candidate] = true

		if _, err := tx.ExecContext(ctx, `UPDATE event_types SET slug = ? WHERE id = ?`, candidate, f.id); err != nil {
			return fmt.Errorf("normalize event type slugs: update %s: %w", f.id, err)
		}
		logger.InfoContext(ctx, "event type slug normalised",
			"event_type_id", f.id, "from", f.slug, "to", candidate)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("normalize event type slugs: commit: %w", err)
	}
	return nil
}
