-- +goose Up
-- Workspace-wide "Show host names on booking pages" switch (Settings -> Branding). ON by
-- default so existing instances keep showing host names/avatars on the public booking,
-- manage and team pages and in the public event-type JSON the embed widget consumes.
-- OFF hides every host identity server-side; only the event/meeting name is shown.
ALTER TABLE server_settings ADD COLUMN show_host_names INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- SQLite doesn't support DROP COLUMN before v3.35; leave the column in place.
