-- +goose Up
-- Workspace-wide calendar invite message (sanitized HTML), appended after the event
-- type's own calendar message on every new booking's invite and on live events. Read at
-- booking time, so changing it applies to every event type from the next booking on;
-- existing calendar events are not rewritten. Empty = none.
ALTER TABLE server_settings ADD COLUMN default_calendar_message TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite cannot easily drop a column; left in place.
