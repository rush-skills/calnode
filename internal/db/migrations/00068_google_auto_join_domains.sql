-- +goose Up
-- Google Workspace domains whose members may sign in without an invite. A Google
-- sign-in whose account belongs to one of these hosted domains (Google's `hd` claim,
-- never the email's text) creates a regular member on first login. Comma-separated,
-- lowercase; empty = off, which is the invite-only behaviour every install had before.
ALTER TABLE server_settings ADD COLUMN google_auto_join_domains TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before v3.35; leave the column in place.
