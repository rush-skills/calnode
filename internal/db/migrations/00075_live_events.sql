-- +goose Up
-- Live events / office hours (docs/features/live-events.md): a workspace-hosted live session
-- whose join link is published on the public status endpoint while it is live and withdrawn
-- when it ends. The host's calendar event (and so its Google Meet link) is stamped with the
-- same external_* triple booking_hosts uses, so updates and cancels route to the provider
-- that wrote it. Timestamps are RFC3339 UTC like the rest of the schema.
CREATE TABLE live_events (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'office_hours',
    status TEXT NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled','live','ended','cancelled')),
    host_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    scheduled_start_at TEXT,
    scheduled_end_at TEXT,
    started_at TEXT,
    ended_at TEXT,
    auto_start INTEGER NOT NULL DEFAULT 1,
    auto_end INTEGER NOT NULL DEFAULT 1,
    join_url TEXT NOT NULL DEFAULT '',
    external_event_id TEXT,
    external_calendar_id TEXT,
    external_provider TEXT,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_live_events_status_start ON live_events (status, scheduled_start_at);

-- +goose Down
DROP INDEX IF EXISTS idx_live_events_status_start;
DROP TABLE IF EXISTS live_events;
