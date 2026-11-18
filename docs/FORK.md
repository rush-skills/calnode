# This fork (rush-skills/calnode)

`develop` is upstream [Calnode](https://github.com/calnode/calnode) plus a set of
additions that run at bookings.ijewel3d.com. This page explains how the fork is laid
out, so that taking upstream's next release stays a routine merge and the additions can
go upstream piece by piece.

## Ground rules

1. **Upstream wins where both have the same thing.** When upstream ships something the
   fork also built, take upstream's version and keep only the part upstream lacks.
   The v0.14 merge did this for event-type admin access (`canManageEventType`,
   `eventTypeForEditor`), booking dispatch (`createHostEvents` / `notifyHosts`),
   reassignment (`ReassignHost`), branding (`internal/branding`) and the admin i18n.
2. **Fork-only code says so.** A comment beginning `fork:` (or a file named in this
   page) marks code upstream does not have. Keep fork additions in their own files
   where possible (`fork.go`, `event_type_access.go`, `migrations_fork/`) and small
   hooks in upstream's files.
3. **No fork numbering in shared sequences.** Migrations are the case that matters; see
   below.

## Migrations: two folders, two goose tables

| Folder | Goose table | Owner |
|---|---|---|
| `internal/db/migrations/` | `goose_db_version` | upstream. **Keep it byte-identical to upstream** (`git diff upstream/main -- internal/db/migrations/` must be empty). |
| `internal/db/migrations_fork/` | `goose_db_version_fork` | this fork, numbered from 00001. |

`db.Migrate` runs upstream's migrations, then the fork's (`internal/db/fork.go`). That
gives three rules.

- **Fork migrations must be additive** (add columns, tables, indexes, triggers; no table
  rebuilds). On a fresh install they run after every upstream migration, including ones
  that came later on an older database.
- **New upstream migrations merge with no renumbering.** They just apply.
- **`/readyz` (`SchemaReady`) checks both sequences.**

### The pre-split conversion (one time)

Before v0.14 the fork's migrations shared upstream's numbers (00068–00082), and two of
them were upstream's own 00068/00069 under fork numbers 00079/00080. On the first boot of
a binary with the split, `prepareForkMigrations` rewrites that bookkeeping in one
transaction. The schema itself is not touched.

| Legacy version | Becomes |
|---|---|
| 68–78 | fork 1–11 |
| 79, 80 | upstream 68, 69 (`invite_delivery`, `booking_attendees` RSVP) |
| 81, 82 | fork 12, 13 |

It only runs when `goose_db_version` exists, there is no fork table yet, and the
fork-only `live_events` table exists. So a fresh install, a pure upstream database and an
already converted one are all left alone. `TestMigrate_convertsPreSplitForkDatabase` covers
it. After the conversion, goose applies upstream's 00070–00072.

### When upstream accepts a fork migration

Databases have already applied the fork copy, so never delete it outright. Do one of
these:

- **(a)** Make upstream's file a no-op there. For example, upstream's migration checks
  whether the column exists, or the column already exists and upstream adds it with
  `IF NOT EXISTS` semantics.
- **(b)** Drop the fork file and add a remap in `fork.go`. This marks the upstream
  version applied when the fork version was.

## Where the fork differs (map for upstream PRs)

Each line is a candidate upstream PR. Detail is in `docs/features/`.

**Backend**

- **Org-wide event types.** `visibility` column, members see org-wide ones read-only, and
  only the owner changes visibility. Files: `event_type_access.go`, `org-events.md`.
  Since v0.14 this sits on upstream's admin model: admins manage every event type,
  private ones included.
- **Calendar invite message and manage links in the event description.** Files:
  `calendar-manage-links.md`, `calendar_message`, `calendarDescription`, and the iCalUID
  capture (`CreateEventParams.ICalUID`).
- **Default participants** invited to every booking. File: `default-participants.md`.
- **CRM sync webhooks.** `bookingsnap` payloads, `revision`, `occurred_at`, `changed`,
  `booking.reassigned` / `booking.updated`, `initiated_by`, secret rotation, redelivery,
  per-event-type filters and read-only API keys. Files: `docs/webhooks.md`,
  `internal/bookingsnap`.
- **Team calendar share links, live events / office hours, booking picker inside a lead
  form.** Files: `team-calendar.md`, `live-events.md`, `docs/embed-in-form.md`.
- **Show-host-names switch, no "Powered by" attribution.** File: `public-pages.md`. The
  `powered_by` locale key is removed, including from upstream's `el`/`pl` files.
- **Google Workspace auto-join and sign-in domains, case-insensitive emails, member
  removal with transfer, any admin may grant admin.**
- **Deleting an event type also deletes its past bookings.** It is a 409 only while
  upcoming bookings exist. Upstream refuses while any booking exists.
- **`GET /v1/bookings/{id}` authorises the caller itself** (credentials or a manage token).

**Admin UI**

- Fork strings use upstream's `$t` catalogue, with keys added to `en.json` and `fr.json`.
  The French strings are drafts, as upstream says of its own. Some fork-only strings
  outside the merged regions are still hard-coded English; translating them is a good
  first upstream PR.
- The event-type editor uses upstream's read-only summary for viewers who cannot edit.
  The fork's disabled-form view was dropped in the merge.
- Rich-text email notes and the calendar message use TipTap (`@tiptap/*` in
  `package.json`).

**Ops**

- `deploy/fly/` (scripted Fly.io deploy; see its README).
- CI and Docker workflows also run on `develop`.

## Taking the next upstream release

```bash
git fetch upstream
git checkout -b merge/upstream-vX develop
git merge --no-ff upstream/main
# migrations/ conflicts should not happen; if they do, take upstream's side
git diff upstream/main -- internal/db/migrations/   # must print nothing
go test ./... && (cd frontend && pnpm install && pnpm check && pnpm check:i18n && pnpm build)
```

Then rehearse on a copy of the production database before deploying: restore the
Litestream backup, boot the new binary against it, and check `/readyz` and the logs.
