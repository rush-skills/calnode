# Public pages: attribution removal + "Show host names"

Two changes to the public booking surfaces (book page, manage page, embed widget,
person/team directory pages). Migration `00071_show_host_names.sql`.

## 1. "Powered by Calnode" attribution removed

The attribution footer is gone from every public surface. Calnode is Apache-2.0 and the
trademark policy has no attribution clause, so there was nothing to keep it for.

- `_shared.html` `legalFooter` partial (book, manage, directory pages): the trailing
  `<a href="https://calnode.com">Calnode</a>` link is removed. The footer itself stays —
  it still carries the operator's privacy/terms links, the cookie-settings button and
  the language switcher — and its class is renamed `.powered-by` → `.legal-footer`
  (in `booking.css` and the partial) so the name says what it is now. The CSS block
  is otherwise unchanged.
- `embed.js`: the `.powered` element appended after the card and its three CSS rules
  are removed. The widget renders no footer of its own now.
- The `powered_by` i18n key is removed from all nine locale files (the same-keys guard
  requires all or none).
- Emails, the LiveKit room and the admin SPA are untouched, as asked.

`TestPublicSurfaces_noPoweredByAttribution` renders book/manage/person pages and
`embed.js`, asserts none contains "powered by"/`calnode.com`, and that the legal footer
(privacy link) is still there.

## 2. Workspace setting: "Show host names on booking pages"

`server_settings.show_host_names INTEGER NOT NULL DEFAULT 1`. Default ON, so upstream
behaviour is unchanged until an operator flips it.

### API

`GET/PATCH /v1/settings/branding` gains `show_host_names` (bool). The PATCH field is a
`*bool`: an omitted key keeps the stored value, so an API client or form that predates
the field cannot switch names off by accident. (The branding editor submits the whole
form; this is the "validate/apply on change, not on mention" rule from CLAUDE.md.)

`loadBranding` scans the column into an int initialised to `1`, so a failed scan (no
settings row yet) leaves the switch ON — the zero value of a bool would have silently
hidden every host.

### What OFF does — enforced server-side

The rule: **no public surface receives a host name or avatar it would then have to
hide.** Every place a name reached a public surface is withheld in Go:

| Surface / payload | ON | OFF |
|---|---|---|
| `book.html` page data (`Hosts`, `HostsLabel`, `SoleHostName`, `HostName`, `HostInitial`, `AvatarURL`) | as before | all empty; `ShowHostNames=false` drops the face/name block from the template |
| `manage.html` page data | as before | `renderManage` blanks the host fields (one place, all callers); template drops avatar + name |
| `GET /v1/event-types/{slug}/public` (`hosts`) | host list | `[]` |
| `GET /v1/event-types/{slug}/slots` (`hosts` id→name map) | map | `{}` (slots keep their opaque `host_ids`) |
| `POST /v1/bookings` response (`hosts`) | assigned hosts | omitted |
| `/team/{slug}` member roster | listed | section omitted entirely |
| `/u/{handle}` | named | **unchanged** — the page is about one named person by construction |
| Emails, calendar invites, admin API, MCP tools, the assistant | unchanged | unchanged |

Why the roster goes entirely rather than being anonymised: an unnamed list of `/u/`
links would still identify people by handle.

Why `/slots` and the create-booking response are included even though the task named
only the page data and the public JSON: `book.html` and `embed.js` learn host names a
second time from those two responses (to narrow the header to the picked slot's host,
and to show the assigned host after booking). Leaving them would have made the page
"hide" names it already held, which is exactly what the server-side rule forbids.

The agent-facing callers (MCP `get_available_slots`/`create_booking`, the booking
assistant) go through `computeSlots`/`createBookingForSlug` directly and are not
affected: this is a public-page presentation setting, not a data-access one.
`dataLayer` events on the pages read `host_name` from the same (now empty) data, so
analytics stop receiving names too.

### Layout with the event name in the title position

- **book.html**: the `#host-faces` div and `#host-name` paragraph are wrapped in
  `{{if .ShowHostNames}}`, so the `<h1 class="event-name">` is the first child of the
  info panel. The page JS keys all host rendering off `#host-faces` existing
  (`renderHosts`/`restoreDefaultHosts` return early when it is absent), so nothing it
  later learns can resurface a face. Desktop column and the mobile step-flow are
  unchanged otherwise.
- **manage.html**: avatar + `.host-name` wrapped the same way.
- **embed.js**: an empty `hosts` on the public payload is the signal. `infoPane` then
  renders no face stack and no host line, **and omits the `.host-faces` container**:
  in the compact (`@container (max-width:719px)`) header the faces column is a flex
  child with a 14px gap, so an empty div would have left the title indented by the
  gap. `emptyDayText` already falls back to the date-only sentence when there is no
  single named host. Slot-narrowing from `hostMeta` is also gated on `hostsShown`,
  so even a stale cached `/slots` map cannot bring a name back.
- `booking.css` is unchanged for this part — no new classes were needed.

### Admin UI

`frontend/src/routes/settings/branding/+page.svelte`: a new "Booking pages" card with
a shadcn `Switch` ("Show host names on booking pages") + help text, saved with the
rest of the form. The page's local `Branding` type is replaced by an exported
`BrandingSettings` in `frontend/src/lib/api.ts` (the task asked for the field there;
the type did not exist yet, so it was moved rather than duplicated — same convention
as `EmailSettings`/`LLMSettings`).

### Tests (`internal/handler/show_host_names_test.go`)

- `TestBrandingSettings_showHostNames_roundTrip`: default true; patch false; a patch
  that omits the key keeps false; patch true.
- `TestBookPage_showHostNamesOff`, `TestManagePage_showHostNamesOff`: page names the
  host by default; with the flag off the body contains the event name and not the
  host's name, and the host block is absent (not just empty).
- `TestPublicEventType_showHostNamesOff`, `TestGetSlots_showHostNamesOff`,
  `TestCreateBooking_showHostNamesOff`: the JSON payloads carry no name; slots and the
  booking itself are unaffected.
- `TestTeamPage_showHostNamesOff`: roster gone, event types still listed, person page
  still named.

### Not done / deliberate

- No "mobile" browser verification was possible in this environment; the layout change
  is structural (the host block is removed, not restyled) and the existing responsive
  rules apply unchanged. Worth a quick look on a phone for each of the three surfaces.
- `gofmt -l` flagged a pre-existing one-line alignment issue in
  `internal/calendar/microsoft/microsoft_test.go`; it is included (whitespace only).
