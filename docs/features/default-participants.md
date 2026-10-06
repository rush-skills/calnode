# Default participants

Workspace-level email addresses that are invited as attendees on the **host's calendar
event** of every meeting booked on the platform. The primary use is a notetaker bot that
auto-joins a meeting when a shared mailbox (e.g. `notes@example.com`) is on the invite.

## Decisions

- **Calendar invite only.** The addresses go on the event each host's connected calendar
  (Google / Microsoft / CalDAV) writes, and that provider's native invite is what reaches
  them. Calnode sends them no email, the booker never sees them (not on the booking page,
  the manage page, the confirmation email, or the booker's `.ics` attachment), and they are
  not part of the booking record. A host with no connected calendar therefore invites no
  one - there is no fallback path, by design.
- **Workspace-wide, not per event type.** One list, admin-only, under Settings →
  Meeting defaults. Per-event-type overrides were deliberately left out of
  the first cut; the column and helpers are shaped so that could be layered on later.
- **Never block a booking.** The list is loaded once per operation through
  `loadDefaultAttendees`; a load failure is logged and the event is created without extras.
- **Max 20, bare addresses only.** `normalizeAttendeeEmails` trims, lower-cases, dedupes
  and validates with `net/mail.ParseAddress`, refusing display names (`Bot <bot@x>`) - the
  stored form is comma-separated and a display name could carry a comma. The error names
  the bad entry so the UI can show it.
- **No double invites.** `extraAttendeesFor(defaults, exclude...)` drops any default that
  equals (case-insensitively) the booker's address or any host's address on the booking.
  The inline create has the hosts in memory; reconcile and reassign load the same set
  through `bookingHostEmails`, so all three paths exclude identical people (on a group
  booking a host who is also a default is not invited to a co-host's event either).
  Providers append what they are given and do no dedupe of their own.
- **CalDAV is best-effort.** The addresses are written as `ATTENDEE;RSVP=TRUE` lines, but
  whether an invite is actually sent depends on the server's RFC 6638 scheduling support
  and on who it considers the organizer (the object keeps the booker as `ORGANIZER`, as
  before). Google and Microsoft send the invite themselves. The UI copy says so. The
  change also removed the `METHOD:REQUEST` line from the stored object, which RFC 4791
  §4.1 forbids in a calendar resource and which, together with `ATTENDEE` lines, strict
  servers would read as an iTIP message and reject. See *Known limits*.

## Storage

Migration `00072_default_attendees.sql`: `server_settings.default_attendee_emails TEXT NOT
NULL DEFAULT ''`, comma-separated, lowercase, normalised on write.

## API

`GET` / `PATCH /v1/settings/participants` (admin only via `requireAdmin`; `PATCH` returns
503 in demo mode like the other settings mutations, and is on the shared `settingsRL`
rate limit).

```json
{"default_attendee_emails": ["notes@example.com"]}
```

`PATCH` takes a pointer slice: omitted keeps the stored list, `[]` clears it. An invalid
entry is a `400` whose message names it. Both verbs echo the normalised stored list.

## Code map

- `internal/handler/default_attendees.go`
  - `(h *Handler) defaultAttendeeEmails(ctx) ([]string, error)` - parses the column
    (lower-casing on read so a hand-written row still meets the invariant); missing
    settings row → `nil, nil`. **Other features call this**; keep the signature.
  - `normalizeAttendeeEmails(in []string) ([]string, error)` - see above; same contract.
  - `extraAttendeesFor`, `loadDefaultAttendees`, `bookingHostEmails` - the booking-path
    helpers.
  - `GetParticipantSettings`, `PatchParticipantSettings`.
- `calendar.CreateEventParams.ExtraAttendees []string` (`internal/calendar/calendar.go`),
  honoured by every provider's `CreateEvent`:
  - `internal/gcal/events.go` - appended to `attendees` after the organizer; rides on the
    existing `sendUpdates=all` so Google emails the invite.
  - `internal/calendar/microsoft/events.go` - `attendees[]` with `type: "required"`.
  - `internal/caldav/events.go` - `ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:…`
    lines after `ORGANIZER` (`buildICS` gained an `attendees` parameter).
- Wired at every place a host calendar event is created:
  - `booking_handler.go` `createHostEventsAndNotify` - every host's event (group bookings
    included); excludes the booker and all hosts of the booking.
  - `calendar_reconcile.go` `reconcileCreations` - healed events (defaults loaded once per
    sweep, host set per booking via `bookingHostEmails`).
  - `reassign.go` - the new host's recreated event (`booking_hosts` already names the new
    host by the time the goroutine runs, so the same helper applies).

### Reschedule (UpdateEvent) - verified, no change needed

None of the three providers rewrites the whole event on update, so attendees are carried
through without any work here:

- Google: `PATCH` with only `start`/`end`.
- Microsoft: `PATCH` with only `start`/`end`.
- CalDAV: `GET` → `rewriteEventTimes` (touches only `DTSTART`/`DTEND`/`DURATION`/
  `DTSTAMP`/`LAST-MODIFIED`/`SEQUENCE`) → `PUT`. Covered by
  `TestRewriteEventTimes_keepsAttendeeLines`.

Cancel deletes the event, which removes it from every attendee's calendar through the
provider's own notification.

## Frontend

`frontend/src/routes/settings/participants/+page.svelte` (shadcn `Textarea`, `Label`,
`Button`; toast on save; `saveOnCmdS`), linked from the Workspace section of
`settings/+layout.svelte` as **Default participants** (`adminOnly`). One address per line;
commas are accepted so a pasted list works. Type: `ParticipantSettings` in `lib/api.ts`.

## Tests

- `internal/handler/default_attendees_internal_test.go` - `normalizeAttendeeEmails` table,
  `extraAttendeesFor`, missing-row → `nil, nil`, and a `reconcileCreations` run asserting the
  healed event's `ExtraAttendees` (host + booker excluded).
- `internal/handler/default_attendees_test.go` - settings round-trip (admin only, omitted
  keeps, `[]` clears, bad entry 400 naming it), booking creation asserting the recorded
  `CreateEventParams.ExtraAttendees` (booker + host excluded; `nil` when nothing is
  configured), and reassign asserting the new host's event carries them.
- `settings_demo_guard_test.go` - the new `PATCH` is in the demo-mode 503 table.
- Provider payloads: `internal/gcal/events_attendees_test.go`,
  `internal/calendar/microsoft/attendees_test.go`, `internal/caldav/attendees_test.go`.

## Known limits / follow-ups

- **CalDAV scheduling servers.** With the booker as `ORGANIZER` and the defaults as
  `ATTENDEE`s, the authenticated host is neither, which a strict RFC 6638 server (Apple
  CalendarServer / iCloud) may refuse to store (`CALDAV:organizer-allowed`). Self-hosted
  sabre-based servers (Nextcloud, Baikal) and Radicale store the object but only send
  iTIP on behalf of an organizer who is the authenticated user, so the bot may receive
  nothing there. The proper fix is to make the host the organizer on CalDAV (and the
  booker an attendee), which needs the host's address on `CreateEventParams` and changes
  who the booker's own invite comes from; deferred, not hidden. Until then an operator
  on CalDAV should verify one booking against their server before relying on the bot.
- **Migration number.** `00072` was assigned by the integration plan while `00067` is the
  newest on main; `goose.Up` runs without allow-missing, so the sibling branches holding
  `00068`-`00071` must land in the same release as this one (or the numbers be made
  contiguous at merge) - a database that applied `00072` first refuses later lower numbers.

- A default participant is invited from **each** host's calendar on a group booking, so a
  three-host meeting produces three invites to the bot. That is how the provider-native
  model works (each host owns their own event) and is harmless for a bot; a human in the
  list will see duplicate invites on group event types.
- Changing the list does not touch events already written; it applies to bookings created
  (or healed / reassigned) after the save.
- The LiveKit room itself does not know about default participants; the bot receives the
  room link via the invite's location like any other attendee.
