-- +goose Up
-- Email domains whose Google/Microsoft sign-ins create a member account on first
-- login (comma-separated, lowercase, no '@'). Empty = invite-only, as before.
ALTER TABLE server_settings ADD COLUMN allowed_signin_domains TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite cannot easily drop a column; left in place.
