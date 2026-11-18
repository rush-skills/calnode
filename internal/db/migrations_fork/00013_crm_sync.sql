-- +goose Up
-- Webhooks and API for an external CRM sync (docs/webhooks.md).

-- Booking revision: a counter that goes up on every change to the booking, its hosts or
-- its attendees, so a receiver can order events whose delivery order is not guaranteed
-- (retries reorder them). changed_at is when the latest change happened, and is what
-- GET /v1/bookings?updated_since= filters on. Kept apart from updated_at, which the
-- booking service owns and Calnode-sent invites use as their SEQUENCE.
ALTER TABLE bookings ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE bookings ADD COLUMN changed_at TEXT NOT NULL DEFAULT '';
-- One fixed format (milliseconds, Z) so changed_at compares correctly as text.
UPDATE bookings SET changed_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%fZ', NULLIF(updated_at, '')),
    strftime('%Y-%m-%dT%H:%M:%fZ', created_at),
    created_at);
CREATE INDEX IF NOT EXISTS idx_bookings_changed_at ON bookings (changed_at);

-- +goose StatementBegin
CREATE TRIGGER bookings_stamp_insert AFTER INSERT ON bookings
BEGIN
    UPDATE bookings SET changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- Any update that did not itself move the revision bumps it. recursive_triggers is off
-- (SQLite's default), so the trigger's own UPDATE does not fire it again; the WHEN
-- clause also keeps the insert stamp above and the child-table triggers below from
-- counting twice.
-- +goose StatementBegin
CREATE TRIGGER bookings_bump_revision AFTER UPDATE ON bookings
WHEN NEW.revision = OLD.revision AND NEW.changed_at = OLD.changed_at
BEGIN
    UPDATE bookings SET revision = OLD.revision + 1,
                        changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER booking_hosts_bump_insert AFTER INSERT ON booking_hosts
BEGIN
    UPDATE bookings SET revision = revision + 1, changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = NEW.booking_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER booking_hosts_bump_update AFTER UPDATE ON booking_hosts
BEGIN
    UPDATE bookings SET revision = revision + 1, changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = NEW.booking_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER booking_hosts_bump_delete AFTER DELETE ON booking_hosts
BEGIN
    UPDATE bookings SET revision = revision + 1, changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = OLD.booking_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER booking_attendees_bump_update AFTER UPDATE ON booking_attendees
BEGIN
    UPDATE bookings SET revision = revision + 1, changed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = NEW.booking_id;
END;
-- +goose StatementEnd

-- The iCalendar UID of each host's calendar event. Note takers (Circleback) identify a
-- meeting by it, so it is the join key between a booking and its transcript.
ALTER TABLE booking_hosts ADD COLUMN external_ical_uid TEXT NOT NULL DEFAULT '';

-- Webhook filter by event type: a JSON array of event type slugs; NULL means all.
ALTER TABLE webhooks ADD COLUMN event_types TEXT;
-- Secret rotation: the previous secret stays valid (deliveries are signed with both)
-- until secret_prev_expires_at, so a receiver can roll without losing a delivery.
ALTER TABLE webhooks ADD COLUMN secret_prev_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE webhooks ADD COLUMN secret_prev_expires_at TEXT NOT NULL DEFAULT '';

-- API key scopes: a JSON array such as ["bookings:read"]; NULL is a full key that acts
-- with its owner's role, as every key did before.
ALTER TABLE api_keys ADD COLUMN scopes TEXT;

-- +goose Down
ALTER TABLE api_keys DROP COLUMN scopes;
ALTER TABLE webhooks DROP COLUMN secret_prev_expires_at;
ALTER TABLE webhooks DROP COLUMN secret_prev_enc;
ALTER TABLE webhooks DROP COLUMN event_types;
ALTER TABLE booking_hosts DROP COLUMN external_ical_uid;
DROP TRIGGER IF EXISTS booking_attendees_bump_update;
DROP TRIGGER IF EXISTS booking_hosts_bump_delete;
DROP TRIGGER IF EXISTS booking_hosts_bump_update;
DROP TRIGGER IF EXISTS booking_hosts_bump_insert;
DROP TRIGGER IF EXISTS bookings_bump_revision;
DROP TRIGGER IF EXISTS bookings_stamp_insert;
DROP INDEX IF EXISTS idx_bookings_changed_at;
ALTER TABLE bookings DROP COLUMN changed_at;
ALTER TABLE bookings DROP COLUMN revision;
