-- +goose Up
-- Rich-text (sanitized HTML) message put on the calendar invite of every booking of
-- this event type, above the "Booking ID" line. Separate from `description`, which is
-- what the public booking page shows. NULL/empty = invite keeps just the booking ID.
ALTER TABLE event_types ADD COLUMN calendar_message TEXT;

-- +goose Down
-- SQLite cannot easily drop a column; left in place.
