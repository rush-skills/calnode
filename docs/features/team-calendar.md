# Team calendar

One calendar of every member's meetings: Calnode bookings for all hosts plus titled
events from each member's connected Google / Microsoft calendars. Readable by any
signed-in member in the admin app (`/admin/team-calendar`), and embeddable in an external
dashboard through a share link that renders a public, frameable page.

## Surfaces

| Surface | Where | Auth |
|---|---|---|
| Data | `GET /v1/team-calendar?from=YYYY-MM-DD&to=YYYY-MM-DD[&token=…]` | session / API key (any member) **or** share token |
| Shares | `GET/POST /v1/team-calendar/shares`, `DELETE /v1/team-calendar/shares/{id}` | admin only |
| Embed page | `GET /embed/team-calendar?token=…` | share token (404 otherwise) |
| Admin page | `frontend/src/routes/team-calendar/+page.svelte` | any member; share card admin-only |

Code: `internal/handler/team_calendar.go` (+ `team_calendar_test.go`), the embed template
`internal/handler/templates/team-calendar.html`, migration `00074_team_calendar_shares.sql`,
`calendar.Provider.ListEvents` with implementations in `internal/gcal/listevents.go` and
`internal/calendar/microsoft/listevents.go`, and `calendar.Service.ListEvents` (fan-out).

## Data endpoint

- `from`/`to` are inclusive calendar dates. `to < from`, a malformed date or a span over
  **42 days** (one six-week month grid) is a 400. The server widens the query window by a
  day each side because the dates are the viewer's *local* grid and the window is UTC; the
  client clips to its own grid. So a 42-day request reads 44 days of data - deliberate.
- `members`: active users (`archived_at IS NULL`), oldest first, each with a colour from a
  12-entry palette **by index**. Ordering by `created_at` is what makes the colour stable:
  a new member is appended and never recolours the others. Archived users are not members
  and their host seats are dropped from the items.
- `items`, sorted by start:
  - **Bookings**: one item per `(confirmed booking, host seat)` from `booking_hosts`,
    with a `UNION ALL` fallback to `bookings.host_id` for pre-`booking_hosts` rows that have
    no seat rows - the same two sources the visibility model reads (ARCHITECTURE §14). The
    item id is `bookingID:userID`, so a two-host booking is two items sharing `booking_id`.
    Title is `"<event type> · <organizer attendee>"`; the parts are also returned separately.
    Full details are intentional: the audience is the whole team, and the admin page is not
    subject to the public `show_host_names` branding flag.
  - **External**: per member with a calendar connection, `Service.ListEvents` over the
    calendars they selected for conflict checking (the same set FreeBusy reads). Events
    whose id matches a platform-written `external_event_id` (on `booking_hosts` or the
    legacy `bookings` column) in the same window are dropped, so a booking is not shown
    twice. A second copy of the same event id for one member (two selected calendars) is
    dropped too. Empty titles render as "Busy".
- **Fan-out**: an `errgroup` with `SetLimit(4)` over the members **that have a calendar
  connection** (`Service.ConnectedUserIDs`, one query), so a 30-person workspace with
  three connected calendars costs three fetches. One member's failure (expired token,
  provider outage) is logged at warn; whatever their other providers returned is kept,
  and the response never fails for it. Every DB row (members, bookings, own event ids,
  connected user ids) is materialised **before** the fan-out starts, because each
  provider's `ListEvents` runs its own queries and the pool is a single SQLite connection
  (ARCHITECTURE §17).
- **Privacy**: an event the owner marked private/confidential (Google `visibility`, Graph
  `sensitivity`) is returned as busy time only - id and times kept, title and location
  blanked, so it renders as "Busy". The member opted that calendar in for *conflict
  checking*; the team calendar must not turn that into a title feed.
- `Cache-Control: no-store` on the data response.

## Provider interface

`calendar.Provider` gained
`ListEvents(ctx, userID, from, to) ([]ExternalEvent, error)`.

- `ExternalEvent{ID, Title, Location, Start, End, AllDay, Source}`. **Deviation from the
  brief**: the struct carries a `Source` field the providers leave empty and
  `Service.ListEvents` stamps with the provider name. Without it the handler would have
  to call providers directly to know whether an event came from Google or Microsoft,
  which is exactly what the Service exists to hide.
- Google: `events.list` with `singleEvents=true&orderBy=startTime&timeMin&timeMax`, paged
  on `nextPageToken` (capped at 20 pages). Cancelled instances are skipped; `date`-only
  boundaries are all-day events at UTC midnight. Reuses `freeBusyConnections`, so the
  token-refresh and calendar-selection handling is the one FreeBusy already has.
- Microsoft: Graph `calendarView` with `$select=id,subject,location,start,end,isAllDay,isCancelled`
  and `Prefer: outlook.timezone="UTC"`, paged on `@odata.nextLink`. Same connection
  helper as FreeBusy (`/me/calendarView` for an unconfigured account, per-calendar
  otherwise).
- CalDAV returns `(nil, nil)`: a REPORT returns full iCalendar bodies whose titles,
  recurrence expansion and timezone handling are their own project. A CalDAV-only member
  still shows their Calnode bookings.
- `Service.ListEvents` routes by the providers stored on `calendar_connections` for that
  user (materialised first), not by asking every registered provider as `FreeBusy` does -
  a provider with no connection for the user would answer nil anyway, but reading the
  rows once is cheaper than N provider-side queries. Unlike `FreeBusy`, which must fail
  closed, it returns the events collected so far **together with** the first provider
  error: a read-only view should not lose a healthy Google account because the same
  member's Microsoft token expired. The handler logs the error and keeps the partial list.

Every fake `Provider` in the tests was given `ListEvents`; the ones that embed
`calendar.Provider` get it for free.

## Share links

`team_calendar_shares(id, name, token_hash UNIQUE, created_by, created_at, revoked_at)`.
The token is `tcs_` + 32 random bytes hex; only its SHA-256 is stored (same helper as API
keys) and the plaintext is returned exactly once from the create call, together with
`embed_url`. Revocation sets `revoked_at` rather than deleting, so the list keeps a record
of what was handed out; the list never includes tokens or hashes.

Security posture of the token: it is accepted by **two** handlers only - the data endpoint
and the embed page - and never by `RequireAuth`, so a leaked token cannot reach any other
API. A present-but-invalid token is rejected without falling through to the session, the
same rule `RequireAuth` applies to a bad API key. The token grants read access to the whole
team's schedule including attendee names; the admin page says so next to the create form.

## Embed page

`GET /embed/team-calendar?token=…` is a server-rendered Go template with inline vanilla JS
and its own inline CSS (same visual family as `book.html`; nothing is shared with
`booking.css` because none of the booking primitives apply). It renders week and month
views, member chips to filter, a detail card on click, and fetches
`/v1/team-calendar?from&to&token=…` with the token the template injected (via
`html/template`'s JS-context escaping).

Headers, set explicitly in `TeamCalendarEmbed`:

- `Content-Security-Policy: … frame-ancestors *` - this page **may be framed by anyone**;
  that is its purpose. It is the only public page that allows framing. It collects nothing,
  so the clickjacking argument that keeps `frame-ancestors 'none'` on the booking pages
  does not apply. **No `X-Frame-Options`** is sent: that header has no allow-list form and
  would override the CSP allow.
- `Cache-Control: no-store`, `Referrer-Policy: no-referrer` (the token is in the URL),
  `X-Content-Type-Options: nosniff`, `<meta name="robots" content="noindex">`.
- Missing, unknown or revoked token → **404** with a small "Calendar not found" page, so
  the URL reveals nothing about whether a share ever existed.

The embed has no viewer preferences, so the week starts on Monday and times use the
browser's locale and time zone (shown in the footer).

## Admin page

`frontend/src/routes/team-calendar/+page.svelte`, sidebar entry "Team calendar" under
Scheduling, visible to every member.

- Week / month views, prev / today / next, member chips (toggle to hide), click an item
  for a shadcn `Dialog` with the details and, for bookings, a button to `/bookings`.
  The week starts on the user's `week_start` preference and times follow `time_format`;
  the time zone is the browser's.
- **The calendar grid is the one deliberate non-shadcn piece**: shadcn-svelte has no
  week/month scheduling grid (its `Calendar` is a date picker), so the grid is hand-built
  with Tailwind utilities and inline styles for colours and positions. Everything around
  it - buttons, badges, the detail `Dialog`, the revoke `ConfirmDialog`, inputs, tooltips -
  is shadcn.
- Admin-only "Share / embed" card: create a named share link, show the token-bearing
  `<iframe>` snippet once with a copy button, list shares (active / revoked) and revoke
  with a ghost icon button + `Tooltip` + destructive `ConfirmDialog`.
- The "Open in Bookings" link goes to the bookings list; the bookings page has no
  deep-link-to-one-booking parameter today, and adding one would have meant editing a
  page another branch is working in. Candidate follow-up.

## Review findings deliberately not applied

- **Migration number 00074 while the tree's latest is 00067.** The number was assigned
  by the integrator (other branches hold 00068-00073). `db.Migrate` runs `goose.Up` without
  `AllowMissing`, so if 00074 is ever applied to a database *before* a lower-numbered
  migration lands, the next boot fails with "missing migrations". Merge order is the
  integrator's call; flagged, not changed.
- **`created_by … ON DELETE CASCADE`** is the schema the brief specified. Consequence:
  deleting the admin who created a share link deletes the link (the iframe goes 404 and
  the row leaves the list). `ON DELETE SET NULL` would keep the link; change if that
  matters.
- `sqlTimeTC` duplicates `booking.sqlTime`. Exporting it would mean renaming a helper used
  throughout `internal/booking/list.go`, a file another branch is active in; left as a
  private copy with a pointer to the original.

## Not done / follow-ups

- No reschedule / cancel from the team calendar; it is read-only by design.
- CalDAV events are not read back (see above).
- The share token is not rate-limited separately from the rest of the public surface.
- The Google and Microsoft `ListEvents` implementations are tested against mock servers
  only; neither has been exercised against a live tenant from this branch.
