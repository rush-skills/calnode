-- +goose Up
-- Reschedule and cancel links on the calendar event.
--
-- When Calnode sends no email, the calendar invite the host's calendar sends is the
-- booker's only message, so it carries the booking's manage links. Those links use a
-- manage token of purpose 'calendar'. A reschedule rotates the 'email' tokens (so a
-- forwarded old confirmation stops working) but must NOT rotate the calendar ones: the
-- calendar event's description is written once, at creation, and is not rewritten on a
-- reschedule, so rotating it would leave a dead link on the very invite it was put on.
ALTER TABLE booking_manage_tokens ADD COLUMN purpose TEXT NOT NULL DEFAULT 'email'
    CHECK (purpose IN ('email', 'calendar'));

-- Organisation-wide webhooks. A 'user' webhook fires for bookings its creator hosts (the
-- original behaviour); an 'org' webhook, which only an admin can create, fires for every
-- booking in the workspace, so one integration hears about the whole team.
ALTER TABLE webhooks ADD COLUMN scope TEXT NOT NULL DEFAULT 'user'
    CHECK (scope IN ('user', 'org'));

-- +goose Down
ALTER TABLE webhooks DROP COLUMN scope;
ALTER TABLE booking_manage_tokens DROP COLUMN purpose;
