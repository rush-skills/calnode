-- +goose Up
-- Whether live_events.join_url came from the host's calendar provider (a minted Meet/Teams
-- link) rather than being typed in. A minted link belongs to the host's calendar event:
-- when the host changes, it is cleared so a fresh one is minted for the new host; a manual
-- link is kept. Rows from before this column are treated as manual (0), which only means
-- an old minted link survives a host change exactly as it did before.
ALTER TABLE live_events ADD COLUMN join_url_minted INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE live_events DROP COLUMN join_url_minted;
