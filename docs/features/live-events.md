# Live events / office hours

A live event is a workspace-hosted session - office hours, a community call, a webinar -
with a lifecycle of **scheduled → live → ended**, or **cancelled**. Whoever starts one is
the host; the workspace's default participants (a notetaker bot, say) are invited on the
host's calendar event automatically; the join link is published on a public status feed
**only while the session is live** and withdrawn the moment it ends. An external app drives
it through the API with an ordinary API key, and shows the state on its own site through a
frameable page or a web component.

Code: `internal/handler/live_events.go` (API + sweep), `live_page.go` (page + widget),
`templates/live.html`, `live-widget.js`, migration `00075_live_events.sql`, admin UI
`frontend/src/routes/live-events/`.

## Decisions

- **The meeting link is the host's calendar event's online meeting.** Creating a session
  creates one event on the host's destination calendar through `calendar.Service.CreateEvent`
  with `AddMeet=true`, `Summary=title`, `Description=description`, and `ExtraAttendees` =
  the workspace default participants minus the host's own address. The returned join URL
  becomes `join_url`. Google mints Meet, Microsoft mints Teams (`providerMintsPlatform`);
  CalDAV cannot mint, so a CalDAV-only host must supply `join_url`.
- **A manual `join_url` is always allowed and always wins.** With one set, the calendar
  event (if any) carries it as `Location` and `AddMeet` is false - never a second link of a
  different kind.
- **Scheduled sessions mint their calendar event at creation**, not at start, so the host's
  and the notetaker's calendars show the session ahead of time. Start reuses that event;
  a schedule change moves it (`UpdateEvent`); a host change cancels it **as the old host**
  (it lives on their calendar) and mints a fresh one on the new host's calendar
  (`rehostLiveEvent`).
- **A minted link belongs to the host's calendar event; a manual one belongs to the
  operator.** `join_url_minted` (migration 00077) records which. On a host change (or
  when the link is cleared) a minted `join_url` is dropped so `ensureLiveEventCalendar`
  mints a fresh Meet/Teams for the new host; a manual `join_url` is kept and carried onto
  the new host's event as `Location`. Typing a link over a minted one marks it manual.
  A live session keeps its link whatever its origin (member removal can re-host a live
  session; the people in the room are not moved).
- **No link, no "live".** `goLive` refuses (**409**) whenever `join_url` is empty after
  the calendar step - a stamped calendar event is not a join link (a personal Microsoft
  account writes the event and mints nothing; a link can also be cleared by PATCH). A
  scheduled session may exist in that state (the link is required at start). A failed
  `start_now` cancels any calendar event it wrote and deletes the row it just inserted, so
  a retrying client never piles up rows or orphaned events it did not ask for.
- **Changing the link re-creates the calendar event.** A PATCH that replaces a minted
  link with a manual one (or clears it) on a scheduled session cancels the event and mints
  a fresh one carrying the new link as `Location` - otherwise the host and the notetaker
  would open the old conference while the public feed publishes the new URL.
- **Transitions are compare-and-set.** `goLive` and `endLive` take a per-event mutex
  (`liveEventLocks`), re-read the row under it, and claim it with
  `UPDATE … WHERE id = ? AND status = 'scheduled'` (`'live'` for end), checked through
  `RowsAffected`. The sweep, a manual start and a cancel can hit one row within a second,
  and the Meet mint in between is a provider round-trip: a second start waits and then
  finds the row live (one calendar event, idempotent 200); a cancel that lands during the
  mint wins - the claim fails, the just-minted event is cancelled again, the link it
  adopted is restored, and the caller gets the row as it is (409 for a cancelled one).
  A cancelled session never becomes live or ended again.
- **A live session keeps its host.** `PATCH host_user_id` on a live row is **409**; end it
  and start a new one. `host_user_id: ""` on any PATCH means "no change" (clients echo the
  create payload), never "reassign to me".
- **End trims the calendar event to the real end** (`UpdateEvent(start, now)`); cancel
  deletes it (`CancelEvent`). Provider errors on end/cancel are logged, never block the
  state change: the session's own row is the source of truth, the calendar mirrors it.
- **Calendar block length.** A session with no scheduled end gets a one-hour calendar
  block (`liveEventDefaultDuration`). That is a hint for the host's day; the sweep's
  auto-end for such sessions is a separate four-hour safety net (`liveEventMaxRunning`).
- **Auth is the existing `RequireAuth`** (session cookie, or API key via `X-API-Key` /
  `Bearer`). No new scheme. Permissions: creator, host or any admin may edit / start / end /
  cancel; every member may read; anyone else gets 403; unknown ids 404. Only admins may
  name another member as host (`host_user_id`); members host themselves.
- **Sweep, not cron.** `StartLiveEventSweeper` runs `SweepLiveEvents(ctx, now)` once at boot
  and every 60 s (same lifecycle as the calendar reconciler). `auto_start` rows past their
  `scheduled_start_at` go live; `auto_end` rows past `scheduled_end_at` (or, with none, 4 h
  after `started_at`) are ended. A due auto-start with **no join link is not retried
  forever**: the sweep turns `auto_start` off, logs a warning, and leaves it to the host to
  start by hand once a link exists. **Stale rows are never auto-started**: only rows with
  `scheduled_end_at` unset or still ahead, and `scheduled_start_at` within the last
  `liveEventMaxRunning` (4 h), go live. An older open-ended row, or one whose end has
  passed (a forgotten entry, a workspace that was down for days), gets `auto_start = 0`
  and a warning instead of publishing a link to a meeting nobody is holding. The function
  takes an explicit clock for tests.
- **Timestamps** are RFC3339 UTC at second precision everywhere in the table (`liveTime`),
  so SQL string comparisons are time comparisons.
- **The status feed is CORS-open (`*`) by design.** It is read-only public data polled by
  the widget from any site, so it is set in the handler and deliberately *not* subject to
  the booking widget's `EMBED_ALLOWED_ORIGINS`. It is `no-store` and rate-limited at
  600/min per IP: a flood guard, not a quota - every viewer polls twice a minute and, behind
  a proxy without `TRUSTED_PROXY_CIDRS`, every viewer shares one bucket, so the slots feed's
  60/min would 429 a page with thirty readers.
- **The `/live` page is frameable** (`Content-Security-Policy: frame-ancestors *`,
  `Cache-Control: no-store`, no `X-Frame-Options`). The booking pages deny framing; this
  page exists to be framed. Its CSP otherwise allows only same-origin fetches and inline
  script/style (the page is self-contained and loads no external assets).
- **The widget is standalone.** `live-widget.js` is a Shadow-DOM web component with its
  own minimal CSS; it does not load `booking.css`, `booking-logic.js` or anything from
  `embed.js`, so neither widget can break the other. Served with the same caching contract
  as `/embed.js` (short max-age, content-hash ETag, CORS-public).

## Deliberate deviations and notes for the integrator

- **Branch base.** This branch was cut from `main` (95d4f17), which predates the
  default-participants merge (7f11fe7). Two consequences, both resolved by that merge:
    `defaultAttendeeEmails` so the package compiles here. **Delete the file when merging**;
    the build fails with "defaultAttendeeEmails redeclared" until you do, which is the
    intended signal. The host-dedupe is done inline in `liveEventExtraAttendees` (the
    branch could not see `extraAttendeesFor`'s signature); switch to that helper if you
    prefer one implementation.
  - `calendar.CreateEventParams.ExtraAttendees` was added here as a field only. The
    Google/Microsoft provider plumbing that turns it into invitees lives in the
    default-participants branch; take that side on conflict.
- **Migration 00075** leaves a gap after 00067 on this base (00068-00074 live on the other
  branches); goose applies ascending versions with gaps fine.
- **Not translated.** `/live` and the widget are English-only, like the LiveKit room: the
  content is three short status strings and viewer-local dates come from `Intl`. Adding
  the strings to the nine locale files is straightforward if the page is ever marketed.
- **`end` on a scheduled session is 409** (it never went live); cancel is the way out.
  `end` and `start` are idempotent on rows already in the target state (200 with the row).
  Cancelling a live session ends it and deletes the calendar event in one provider call
  (the end-time trim is skipped, since the event is removed right after).
- **`start_now` with a `scheduled_end_at` in the past is 400** (server and dialog): the
  sweep would otherwise auto-end the brand-new session within a minute.

## API reference (for the external service)

All management endpoints require `RequireAuth`: send `X-API-Key: <key>` (or
`Authorization: Bearer <key>`) of a member or admin. Errors are `{"error": "..."}`.
All timestamps are RFC3339; the server stores UTC.

### Live event object

```json
{
  "id": "…", "title": "Office hours", "description": "", "kind": "office_hours",
  "status": "scheduled|live|ended|cancelled",
  "host_user_id": "…", "host_name": "Ada",
  "scheduled_start_at": "2030-01-01T10:00:00Z", "scheduled_end_at": null,
  "started_at": null, "ended_at": null,
  "auto_start": true, "auto_end": true,
  "join_url": "https://meet.google.com/abc-defg-hij", "join_url_minted": true,
  "has_calendar_event": true,
  "created_by": "…", "created_at": "…", "updated_at": "…"
}
```

### `POST /v1/live-events` → 201

| field | notes |
|---|---|
| `title` | required, ≤ 200 chars |
| `description` | optional, ≤ 4000 chars |
| `kind` | `office_hours` (default) or `event` |
| `scheduled_start_at`, `scheduled_end_at` | optional RFC3339; end must be after start |
| `start_now` | `true` creates and goes live immediately |
| `host_user_id` | admins only; default = caller. Members get 403 if they name anyone else |
| `join_url` | manual http(s) link; overrides minting |
| `auto_start`, `auto_end` | default `true` |

A scheduled session mints its calendar event (and link) now when the host has a calendar;
otherwise it is created without one. `start_now` replies **409** when no link can be
produced (no row is left behind).

### `GET /v1/live-events?status=&limit=` → `{"live_events": [...]}`

Live first (most recently started first), then scheduled by start (unscheduled last),
then ended/cancelled most recent first. `limit` defaults to 50, max 200.

### `GET /v1/live-events/{id}` → the object (any member).

### `PATCH /v1/live-events/{id}` → 200

Same fields as create (no `start_now`). Validated on change only. Moving the schedule
updates the calendar event; changing the host or the `join_url` of a scheduled session
re-creates it (new host's calendar / new link as location; a minted link is re-minted for
the new host, a manual one travels with the session - see `join_url_minted` in the
object). 409 on an ended or cancelled
session, and on a host change while live. `host_user_id: ""` is "no change". A live
session cannot have its `join_url` cleared.

### `POST /v1/live-events/{id}/start` → 200

Goes live now (`started_at`), minting the calendar event if there is none. Host = the
stored host, else `{"host_user_id": "..."}` from the body (admins), else the caller.
Idempotent on a live row; **409** without a join link, or on an ended/cancelled row.

### `POST /v1/live-events/{id}/end` → 200

Ends now (`ended_at`), sets the calendar event's end to now. Idempotent on an ended row;
409 on a scheduled or cancelled row.

### `DELETE /v1/live-events/{id}` → 200

Marks the row cancelled (a live one is ended first) and deletes its calendar event.
Idempotent.

### `GET /v1/live/status?kind=` - public, no auth

```json
{
  "live": [{ "id", "title", "description", "kind", "host_name", "join_url", "started_at" }],
  "next": { "id", "title", "description", "kind", "host_name", "scheduled_start_at", "scheduled_end_at" } | null,
  "upcoming": [ ...same shape as next, up to 5 ]
}
```

`join_url` appears **only** on `live` rows. `next` is the earliest scheduled session whose
window has not fully passed: until its `scheduled_end_at`, or for four hours after its
start when it has none (so a session past its start but not yet live - auto_start off,
host running late - is still "next"). `upcoming` is the sessions after it (it does not
repeat `next`). `kind` filters all three.
Headers: `Access-Control-Allow-Origin: *`, `Cache-Control: no-store`. 600 requests/min/IP.

### `GET /live?kind=&theme=light|dark` - public page

Self-contained HTML that polls the status feed every 30 s (and on tab focus) and shows a
pulsing LIVE badge, title, host and a Join button, or "Offline" with the next session in the
viewer's local time plus the upcoming list. Frameable from any origin.

```html
<iframe src="https://cal.example.com/live" width="100%" height="320" style="border:0"></iframe>
```

### `GET /live-widget.js` - public web component

```html
<script src="https://cal.example.com/live-widget.js" async></script>
<calnode-live data-kind="office_hours" data-poll="30"></calnode-live>
```

`data-base` defaults to the script's own origin; `data-kind` omitted shows every kind;
`data-poll` is seconds (min 5, default 30). The element dispatches `calnode-live:update`
with the status payload as `detail` after every successful poll, for sites that want to
react to it themselves.

## Admin UI

`/admin/live-events` (sidebar → Scheduling → Live events, every member): list with status
badges (live pulses), host, schedule in the viewer's zone, copy-join-link; "New live event"
dialog (title, description, kind, optional start/end via DatePicker + time, "Start now",
host picker for admins, manual link, auto flags); per-row Start / End / Cancel (End and
Cancel through `ConfirmDialog`, Cancel destructive); an Embed card with copyable iframe and
widget snippets. The list re-polls every 30 s so sweep transitions show without a reload.

## Tests

`internal/handler/live_events_test.go` (state machine, validation, 409 paths, calendar
minting with a fake provider, manual override, permissions matrix, public status payload,
sweep with a fixed clock, page headers, widget headers) and
`live_events_internal_test.go` (default participants minus host on `ExtraAttendees`).
