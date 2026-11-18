-- +goose Up
-- Recurring live events (docs/features/live-events.md, "Recurring series"). A series is a
-- weekly rule in the host's own timezone ("Tuesdays and Thursdays 17:00 Europe/Sofia, every
-- week, skipping 21-27 Dec"); the sweep materialises each occurrence a few weeks ahead as
-- an ordinary live_events row with its own id, so the public feed, the widget and the
-- start/end/cancel machinery need nothing new. Dates and times in the rule are wall-clock
-- in `timezone`, so 17:00 stays 17:00 across daylight saving.
CREATE TABLE live_event_series (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'office_hours',
    -- JSON array of user ids. One entry = a fixed host; several rotate per occurrence.
    host_user_ids TEXT NOT NULL DEFAULT '[]',
    timezone TEXT NOT NULL,
    -- JSON array of weekdays, 0 = Sunday .. 6 = Saturday.
    weekdays TEXT NOT NULL,
    interval_weeks INTEGER NOT NULL DEFAULT 1 CHECK (interval_weeks >= 1),
    start_time TEXT NOT NULL,          -- "HH:MM", wall clock in timezone
    duration_minutes INTEGER NOT NULL CHECK (duration_minutes > 0),
    start_date TEXT NOT NULL,          -- YYYY-MM-DD in timezone, first possible date
    end_date TEXT,                     -- YYYY-MM-DD inclusive; NULL = open-ended
    -- JSON array of "YYYY-MM-DD" or "YYYY-MM-DD/YYYY-MM-DD" (inclusive range).
    skip_dates TEXT NOT NULL DEFAULT '[]',
    -- A link every occurrence reuses (a standing Meet room). '' = each occurrence gets a
    -- fresh one from its host's calendar.
    join_url TEXT NOT NULL DEFAULT '',
    auto_start INTEGER NOT NULL DEFAULT 1,
    auto_end INTEGER NOT NULL DEFAULT 1,
    invite_default_participants INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- An occurrence remembers its series and the date it stands for (the key that stops the
-- sweep materialising the same date twice, and that keeps a cancelled date cancelled).
ALTER TABLE live_events ADD COLUMN series_id TEXT REFERENCES live_event_series(id) ON DELETE SET NULL;
ALTER TABLE live_events ADD COLUMN occurrence_date TEXT;
-- Comma list of the field groups edited on this occurrence alone (title, description, kind,
-- schedule, host, link, auto, invite): a later edit of the series leaves those alone.
ALTER TABLE live_events ADD COLUMN overrides TEXT NOT NULL DEFAULT '';
-- Whether the workspace's default participants (a notetaker, say) are invited on the
-- host's calendar event. Sessions promised as "not recorded" turn it off.
ALTER TABLE live_events ADD COLUMN invite_default_participants INTEGER NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX idx_live_events_series_date ON live_events (series_id, occurrence_date)
    WHERE series_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_live_events_series_date;
ALTER TABLE live_events DROP COLUMN invite_default_participants;
ALTER TABLE live_events DROP COLUMN overrides;
ALTER TABLE live_events DROP COLUMN occurrence_date;
ALTER TABLE live_events DROP COLUMN series_id;
DROP TABLE IF EXISTS live_event_series;
