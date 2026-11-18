# Live events / office hours

A live event is a workspace-hosted session - office hours, a community call, a webinar -
with a lifecycle of **scheduled → live → ended**, or **cancelled**. Whoever starts one is
the host; the workspace's default participants (a notetaker bot, say) are invited on the
host's calendar event automatically; the join link is published on a public status feed
**only while the session is live** and withdrawn the moment it ends. An external app drives
it through the API with an ordinary API key, and shows the state on its own site through a
frameable page or a web component.

Code: `internal/handler/live_events.go` (API + sweep), `live_page.go` (page + widget),
`templates/live.html`, `live-widget.js`, migration `migrations_fork/00008_live_events.sql`, admin UI
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
  operator.** `join_url_minted` (fork migration 00010) records which. On a host change (or
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
- **Migration numbering:** the fork's migrations now live in `internal/db/migrations_fork` with their own goose table, so they never collide with upstream's numbering; see docs/FORK.md.
- **Translated, in every booker locale.** The strings are the `live_*` keys in
  `internal/i18n/locales/*.json` (LLM drafts outside English, like the rest of the booker
  locales). `/live` resolves its language like the booking page (`?lang=`, the
  `calnode_lang` cookie, `Accept-Language`, the operator's fallback locale), renders its
  static text through `.T`, sets `<html lang>`, and hands its script one object of finished
  strings (`liveLabels`) plus the locale tag its `Intl` calls use. `?lang=` is *not*
  persisted to the cookie here: on a framed page it is the embedding site's setting, not
  the visitor's choice. The widget cannot use templates and is one static file for every
  language, so it asks the status feed instead (`?lang=` → `locale` + `labels`, below).
  Neither surface builds grammar client-side: the only substitution is `%s` / `%[n]s` (a
  mirror of `BookingLogic.fmt`) for values that arrive after load - a host name, a session
  title, a time `Intl` formatted in the viewer's zone. The two built-in kinds
  (`office_hours`, `event`) are translated (`live_kind_*`); a custom kind is shown as the
  operator named it. Guards: `TestLiveSurfaces_i18nKeysExist` (every lookup in the page and
  widget is sent and defined; the widget's English fallback equals `en.json`).
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
  "has_calendar_event": true, "invite_default_participants": true,
  "series_id": null, "occurrence_date": null, "overrides": [],
  "created_by": "…", "created_at": "…", "updated_at": "…"
}
```

### `POST /v1/live-events` → 201

| field | notes |
|---|---|
| `title` | required, ≤ 200 chars |
| `description` | optional, ≤ 4000 chars |
| `kind` | default `office_hours`; any short lowercase slug (`webinar`, `weekly_onboarding`) |
| `invite_default_participants` | default `true`; `false` keeps the workspace's default participants (a notetaker) off the calendar event |
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

### `GET /v1/live/status?kind=&limit=&lang=` - public, no auth

```json
{
  "live": [{ "id", "title", "description", "kind", "host_name", "join_url", "started_at",
             "scheduled_start_at", "scheduled_end_at", "series_id" }],
  "next": { "id", "title", "description", "kind", "host_name", "scheduled_start_at", "scheduled_end_at", "series_id" } | null,
  "upcoming": [ ...same shape as next, up to `limit` ]
}
```

- `scheduled_start_at` / `scheduled_end_at` on a live row are its planned window (omitted
  when unset), so a site can say "until 18:00".
- `series_id` is set on an occurrence of a recurring series (omitted on a one-off). Every
  occurrence has its own `id`, so a client dedupes by `id`.
- `limit` is how many sessions `upcoming` may hold after `next`: default 5, max 50.
- `kind` is one kind or a comma list (`kind=webinar,office_hours,weekly_onboarding`).

`join_url` appears **only** on `live` rows. `next` is the earliest scheduled session whose
window has not fully passed: until its `scheduled_end_at`, or for four hours after its
start when it has none (so a session past its start but not yet live - auto_start off,
host running late - is still "next"). `upcoming` is the sessions after it (it does not
repeat `next`). `kind` filters all three.

The 600/min limit is per client IP. Behind Fly.io set `TRUSTED_PROXY_CIDRS=172.16.0.0/12`
and `CLIENT_IP_HEADER=Fly-Client-IP` (both in `deploy/fly/.env.example`): the request
crosses two Fly proxy hops, and without the header the X-Forwarded-For walk can stop on a
shared edge address. The request log's `client_ip` shows the address the limit keys on.
Headers: `Access-Control-Allow-Origin: *`, `Cache-Control: no-store`. 600 requests/min/IP.

**`lang` (optional) adds two fields and changes nothing else.** Without it the payload is
exactly the shape above. With it:

```json
{
  "live": [...], "next": {...}, "upcoming": [...],
  "locale": "fr",
  "labels": {
    "live_now": "En direct", "live_offline": "Hors ligne", "live_join_now": "Rejoindre",
    "live_hosted_by": "Animé par %s", "live_next_session_at": "Prochaine session : %s",
    "live_today_at": "Aujourd’hui à %s", "live_date_at": "%s à %s", "…": "…"
  }
}
```

`lang` is a BCP-47 tag. An exact Calnode locale code wins (`fr-CA`); otherwise it is
matched like a one-entry `Accept-Language`, so `pt-BR` gets `pt` and `de-AT` gets `de`; no
match gets the operator's fallback locale (English by default). `locale` is the code
actually used - pass it to `Intl` so day and month names agree with the labels. The
visitor's own `Accept-Language` is not consulted: a widget on someone else's page should
match that page. `labels` holds every `live_*` key (`liveLabelKeys`); values are finished
strings, and the only placeholders are `%s` / `%[n]s` for the title, host name or
formatted time the client drops in.

### `GET /live?kind=&theme=light|dark&lang=` - public page

Self-contained HTML that polls the status feed every 30 s (and on tab focus) and shows a
pulsing LIVE badge, title, host and a Join button, or "Offline" with the next session in the
viewer's local time plus the upcoming list. Frameable from any origin. Translated: `lang`
forces a locale (`/live?lang=de`), otherwise the visitor's `Accept-Language` picks one.

```html
<iframe src="https://cal.example.com/live" width="100%" height="320" style="border:0"></iframe>
```

### `GET /live-widget.js` - public web component

```html
<script src="https://cal.example.com/live-widget.js" async></script>
<calnode-live data-kind="office_hours" data-poll="30" data-lang="fr"></calnode-live>
```

`data-base` defaults to the script's own origin; `data-kind` omitted shows every kind;
`data-poll` is seconds (min 5, default 30). `data-lang` picks the language; without it the
widget uses the nearest `lang` attribute (normally the host page's `<html lang>`), then
`navigator.language`. It sends that as `?lang=` on every poll and renders the returned
`labels`; before the first answer it shows a language-neutral "…", and if the server never
answers it falls back to its built-in English. The script itself is identical for every
language, so its content-hash ETag and caching are unchanged. The element dispatches `calnode-live:update`
with the status payload as `detail` after every successful poll, for sites that want to
react to it themselves.

## Recurring series (fork)

Code: `internal/handler/live_series.go`, fork migration `migrations_fork/00014_live_event_series.sql`,
admin UI on `/admin/live-events` ("New recurring series").

A series is a weekly rule, never a session itself. The sweep turns it into ordinary
`live_events` rows four weeks ahead (`liveSeriesHorizon`), one per date, each with its own
`id`. Everything above (the status feed, `/live`, the widget, calendar minting, start/end/
cancel, auto start/end) works on an occurrence exactly as on a one-off session.

- **Wall clock in the series' timezone.** `start_time` + `timezone` (IANA) give each date's
  start, so 17:00 Europe/Sofia is 14:00Z in summer and 15:00Z in winter. Dates in the rule
  (`start_date`, `end_date`, `skip_dates`) are calendar dates in that zone.
- **Rule:** `weekdays` (0 = Sunday … 6 = Saturday), `interval_weeks` counted from the week
  (Monday-based) of `start_date`, `duration_minutes`, optional inclusive `end_date`,
  `skip_dates` as `YYYY-MM-DD` or inclusive ranges `YYYY-MM-DD/YYYY-MM-DD`.
- **Hosts:** `host_user_ids` is one host or a rotation. The n-th held occurrence goes to
  `hosts[n % len]`, counting from `start_date` and skipping skipped dates, so a skipped
  week does not skip a host. Members may only host their own series; admins may name anyone.
- **Link:** `join_url` set = a standing room every occurrence reuses (manual link, carried on
  each calendar event as Location, nothing minted). Empty = each occurrence gets a fresh
  Meet/Teams from its host's calendar, minted when the occurrence is materialised.
- **One occurrence on its own.** `PATCH /v1/live-events/{id}` works as for any session and
  records which field groups it changed in `overrides` (`title`, `description`, `kind`,
  `schedule`, `host`, `link`, `auto`, `invite`). A later series edit updates every future
  scheduled occurrence field by field and leaves those groups alone. `DELETE` cancels that
  date only; the cancelled row keeps the date taken, so it is never re-created.
- **Series edits** (`PATCH /v1/live-series/{id}`) touch only *future scheduled* occurrences:
  live, ended and cancelled ones are history. A date the rule no longer produces (newly
  skipped, past a shortened end, a removed weekday) is deleted along with its calendar
  event; nobody has its link yet. A new host re-hosts the occurrence (old event cancelled
  as the old host, fresh one minted); a changed title, description, link or guest list
  re-creates the calendar event (a minted link is minted afresh); a moved time moves it.
- **Ending a series** (`DELETE /v1/live-series/{id}`) stops it producing dates and cancels
  its scheduled occurrences. A session live right now keeps running.
- **Member removal:** transfer mode puts the new member in the leaver's place in every
  rotation; delete mode drops them, and a series left with no host ends.
- **Not recorded:** `invite_default_participants` (on series and on single sessions,
  default true) keeps the workspace's default participants, such as a notetaker, off the
  calendar event. Changing it on a scheduled session re-creates its calendar event.

### `POST /v1/live-series` → 201 `{"series": {...}, "occurrences": [...]}`

| field | notes |
|---|---|
| `title` | required, ≤ 200 chars |
| `description` | optional, ≤ 4000 chars |
| `kind` | default `office_hours`; any short lowercase slug (`webinar`, `weekly_onboarding`) |
| `timezone` | required, IANA name, e.g. `Europe/Sofia` |
| `weekdays` | required, non-empty, 0–6 |
| `interval_weeks` | default 1 (1–52) |
| `start_time` | required, `HH:MM` 24-hour |
| `duration_minutes` | required, 1–1440 |
| `start_date` | required, `YYYY-MM-DD` |
| `end_date` | optional; `""` on PATCH clears it |
| `skip_dates` | optional list of dates / ranges |
| `host_user_ids` (or `host_user_id`) | default the caller |
| `join_url` | `""` = fresh link per occurrence |
| `auto_start`, `auto_end`, `invite_default_participants` | default `true` |

`GET /v1/live-series?status=active|ended` → `{"series": [...]}` (any member).
`GET /v1/live-series/{id}` → the series with its scheduled and live occurrences.
`PATCH /v1/live-series/{id}` → same fields; applied as above. 409 on an ended series.
`DELETE /v1/live-series/{id}` → ends it. Creator, any host of the series, or an admin may
manage it; any member may read.

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
sweep with a fixed clock, page headers, widget headers),
`live_events_internal_test.go` (default participants minus host on `ExtraAttendees`),
`public_pages_i18n_test.go` (`/live` in French by `Accept-Language` and German by `?lang=`;
the feed's `labels` only with `lang`, and its matching rules) and
`public_pages_i18n_internal_test.go` (every string lookup is sent and defined).
