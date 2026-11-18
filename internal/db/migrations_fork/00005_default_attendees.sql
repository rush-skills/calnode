-- +goose Up
-- Workspace-level default participants: email addresses invited as attendees on the HOST'S
-- calendar event of every booking (typical use: a notetaker bot that joins when a shared
-- mailbox is invited). Comma-separated, lowercase, normalised on write by the settings API.
-- Calendar invite only - Calnode never emails them and bookers never see them.
ALTER TABLE server_settings ADD COLUMN default_attendee_emails TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before v3.35; leave the column in place.
