-- +goose Up
-- Who inside the workspace can see (and, for admins, edit) an event type in the admin
-- UI. 'org' = every signed-in member sees it and admins may edit it; 'private' = only
-- the owner (and anyone assigned as a host, read-only) sees it.
--
-- The default is 'org' on purpose: every event type that exists today becomes
-- organisation-visible. Until now a member could only see event types they owned or
-- hosted, which made a shared instance look empty to everyone but the person who set
-- it up. Hiding one again is a per-event-type choice ("Only me") the owner makes.
--
-- Unrelated to is_public, which governs the PUBLIC directory pages, not the admin UI.
ALTER TABLE event_types ADD COLUMN visibility TEXT NOT NULL DEFAULT 'org'
  CHECK (visibility IN ('org', 'private'));

-- +goose Down
ALTER TABLE event_types DROP COLUMN visibility;
