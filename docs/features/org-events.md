# Org-wide event types and slug normalisation

Feature branch notes: what was built, the decisions behind it, and where it deliberately
deviates from the brief. Migration: `00073_event_type_visibility.sql`.

## A. Slug normalisation

**Problem.** `CreateEventType` stored the slug exactly as typed. The update path already ran
`slugify()`, so `PATCH` produced canonical slugs while `POST` happily stored `"Intro Call"`,
whose `/book/Intro Call` link never resolved.

**Create (`internal/handler/event_type.go`).**
- `slug := slugify(req.Slug)`; when the raw slug is blank, `slugify(req.Name)` instead (same
  as teams). Name is required and trimmed.
- Empty after normalising → `400 "slug must contain letters or numbers"`. A slug that was
  *given* but unusable (`"!!!"`) is a 400 rather than a fallback to the name: the caller said
  what they wanted and it cannot be honoured. Same rule as the update path.
- Duplicate → `409 "slug already in use"` via the existing UNIQUE constraint; two spellings
  that normalise to the same slug now collide, as they should.
- `slugify` is the only normaliser; nothing else is rejected (unicode is dropped, not refused).

**Startup sweep (`internal/handler/event_type_slugs.go`).** `NormalizeEventTypeSlugs(ctx, db,
logger)` rewrites every stored slug where `slugify(slug) != slug`, oldest first, logging
`event_type_id`/`from`/`to`. Collisions get `-2`, `-3`, … (the duplicate endpoint's `-copy-N`
convention). A slug with nothing usable falls back to `slugify(name)`, then to `"event-type"`.
Canonical slugs are never touched; a second run is a no-op. It is wired into
`server.BuildHandler` (`internal/server/server.go`), which runs after `db.Migrate` in
`cmd/calnode/main.go`; a failure is logged, not fatal.

**Deviation from the brief (deliberate): rows with bookings are skipped.** The brief assumed
non-canonical links were already broken. They were not: `/book/{slug}`, `/slots` and
`CreateBooking` match the stored slug byte-for-byte, so `Intro_Call` resolved, and even
`Intro Call` did once a browser percent-encoded it. A booking is proof the link reached someone
(a manage link in their inbox, an embed on a customer site), and renaming it is exactly what
`PatchEventType` refuses with a 409. So the sweep rewrites only rows with zero bookings and logs
the rest at `warn` (`event_type_id`, `slug`, `canonical`, `bookings`) for the operator to
decide. To keep those rows editable, `PatchEventType` now treats a resubmitted slug as a rename
only when its canonical form differs from the canonical form of the stored slug — the editor
resubmits the whole form, and `"Intro_Call"` vs `"intro-call"` is not the operator asking for a
rename (CLAUDE.md: validate on change, not on mention). Covered by
`TestNormalizeEventTypeSlugs_sweep`.

Bookings reference event types by id, so nothing else moves. A canonical candidate can never
equal a non-canonical stored slug (one is a fixed point of `slugify`, the other is not), so the
sweep only needs the set of untouched slugs plus its own rewrites to avoid collisions. A sweep
failure is logged, not fatal: with the PATCH tolerance above, an un-swept row costs nothing but
a cosmetic slug.

**Frontend.** `frontend/src/lib/slug.ts` is a commented mirror of the Go `slugify` (display
only). The create form shows `Booking link: /book/<preview>` live (slug falls back to the name,
exactly as the server derives it) and the slug field is now optional; the editor shows
"Will be saved as /book/…" whenever the typed slug differs from its normalised form. Both
surfaces use the slug the server returns.

## B. Org-wide event types

**Schema.** `event_types.visibility TEXT NOT NULL DEFAULT 'org' CHECK (visibility IN ('org',
'private'))`. The default converts every existing row to org-visible (required by the brief;
verified by `internal/db/event_type_visibility_migration_test.go`, which migrates to 00072,
inserts, then migrates on). `visibility` is unrelated to `is_public`, which governs the public
directory pages; the editor now labels that checkbox "Listing" to keep the two apart.

**The rule (`internal/handler/event_type_access.go`).**
- *See:* org-wide, your own, or one you are assigned to host (`eventTypeVisibleFilter`).
  Hosts-see-private is kept from the old model: someone hosting an event type needs to know
  what they are hosting, and the pre-existing `TestEventTypes_assignedHostSeesReadOnly` still
  covers it.
- *Edit:* `canEditEventType(user, ownerID, visibility)` = owner, or admin && org-wide.
  Private stays owner-only, so an admin cannot reach into something a member kept to
  themselves.
- `eventTypeIDForEditor` / `eventTypeForEditor` replace **every** former
  `WHERE slug = ? AND user_id = ?` write guard and `eventTypeIDForOwner` (deleted): PATCH,
  DELETE, hosts PUT, questions POST/PATCH/DELETE, duplicate, test-email. 404 for an unknown
  slug; **403** for "exists but not yours". 403 rather than 404 is deliberate — the slug is
  the public booking URL, so its existence is no secret, and a member who just saw the event
  type in their list deserves to be told why the save failed.
- `eventTypeIDForViewer` backs the admin-side *reads* the editor needs (hosts list, admin
  questions list) so a read-only viewer sees the same panels an editor does; 404 when the
  event type is private to someone else.
- Admin on someone else's **private** event type: reads 404 (private means not there, as far
  as the rest of the workspace is concerned), writes 403 (the brief's matrix asks for it, and
  the slug is public anyway). The two answers differ on purpose and are both tested; a reviewer
  flagged the asymmetry, and it is accepted rather than leaking private rows into admin reads.

**JSON.** `eventTypeJSON` gains `visibility`, `owner_id`, `can_edit`; `owner_name` /
`owner_email` are now populated on list and get for everyone (they already were for
non-owners, and member emails are visible via `/v1/users` anyway). `owned` and `can_edit` are
set in one place (`stampViewer`) so they can never disagree with the write-side rule. The
`etColumns` list carries `user_id, visibility`, so every scan picks them up.

**Create.** Any member; `visibility` optional, default `org`, validated; the creator stays
`user_id`.

**PATCH.** `visibility` is validated and — compared against the *stored* value, never merely
mentioned — only the owner may change it (`403 "only the owner can change who an event type
is visible to"`). An admin resubmitting the editor's whole form, visibility unchanged, saves
fine. Rationale: an admin may edit a shared event type, but hiding it from (or on behalf of)
its creator is the creator's call. Location validation now runs against the **owner's**
connections (`ref.OwnerID`), not the caller's: auto-generated Meet/Teams/Zoom links are
minted from the owner's calendar at booking time. The UPDATE and re-read key on `id`.

**Duplicate.** The copy keeps the source's `user_id` and `visibility` (both now in the
`INSERT … SELECT`; the column drift-gate test lists `visibility` under "copied"). So an admin
duplicating a colleague's org-wide event type gets a copy that colleague still owns — whose
calendar mints its links and whose hosts/availability rules it inherited — and that the admin
can still edit because it is org-wide. Making the admin the owner would detach the copy from
the hosts and rules it was copied with.

**Transfer (deviation, deliberate).** `TransferEventType` is untouched. It is admin-only *and*
requires `expected_owner_id == caller`, i.e. the caller is the owner; its semantics (move
ownership to a required host, no upcoming bookings) are a separate flow with its own tests. The
editor now shows the Transfer section only to an admin who is also the owner, which is the
only caller the endpoint accepts anyway.

**Unchanged on purpose.** `/book/{slug}`, the directory pages, MCP tools, `archive.go`'s
"deactivate a departing user's event types" (keyed on `user_id`, which still means owner),
and per-event-type availability rules (keyed on the host's own `user_id`, not ownership).

**Frontend.**
- List (`routes/event-types/+page.svelte`): everything visible; `by <owner>` badge when not
  owned; `Org-wide` / `Only me` badge always; Active switch, duplicate, archive, delete only
  when `can_edit`; the gear opens the editor for everyone (tooltip says read-only when so);
  everyone gets open + a new copy-link button.
- Editor (`routes/event-types/[slug]/+page.svelte`): when `!can_edit`, a "View only — created
  by X" notice (the editor's existing amber notice pattern; there is no shadcn Alert component
  in this repo, and adding one to `ui/**` would pull in the `pnpm test:visual` obligation for
  a one-off), the whole form wrapped in one native `<fieldset disabled>` — every shadcn
  control honours `:disabled`, so inputs, selects, checkboxes, switches and buttons go
  read-only at once — Save hidden and Cmd+S guarded. The Embed tab stays outside the fieldset
  so the snippet can still be copied. A "Visibility in the admin" shadcn Select (Whole
  organisation / Only me) sits on the General tab, disabled for non-owners with an explanatory
  hint. Meeting-link hints show a neutral sentence for a non-owner editor, since the connected
  calendar/Zoom status endpoints describe the caller, not the owner. The Hosts tab's
  "Just me / Every booking goes to you" copy names the owner when the viewer is not them, and
  — the one real bug a review caught — the fixed-routing save PUTs `et.owner_id` as the
  required host, not `$currentUser.id`: an admin fixing a typo on a colleague's event type
  must not quietly become its host.

## Review log

Reviewed with the `code-review` skill (Fable 5.1, high) and a separate Opus 5.5 session on
the full patch. Fable findings: admin save swapping the host list (fixed), sweep renaming
working links (fixed: skip booked rows + PATCH tolerance), non-Latin name → confusing 400
(fixed: error names the name field), whitespace-only slug blocking the create form (fixed),
owner-blind Hosts-tab copy (fixed), admin-on-private 404/403 asymmetry (accepted, above), and
two correlated owner subqueries in the list query (not changed: SQLite does them as PK lookups
over a few dozen rows, and a JOIN would force qualifying every column in `etColumns`).
- `api.ts`: `EventType` gains `visibility`, `owner_id`, `can_edit`.

## Tests

Go (`internal/handler`): `event_type_slug_normalize_test.go` (spaces/case/underscore/unicode
create, derive-from-name, 400 on nothing usable, 409 after normalising, sweep with collision
suffixing + fallbacks + idempotence + the rewritten slug resolving on `/book/`);
`event_type_org_test.go` (list/get for owner, member, admin on org and private; write matrix
over update/hosts/questions/duplicate/delete × owner/admin/member × org/private + unknown
slug; question update/delete and hosts-list scopes; owner-only visibility change; admin
duplicate keeps owner; column default). `internal/db`: the migration default on a
pre-existing row and the CHECK. Three existing tests were updated to the new contract
(403 instead of 404 for a visible-but-not-editable write; `book_test` fetches the page by the
slug the server returned; the duplicate drift gate lists `visibility`).

Real app: built the binary with the embedded SPA, ran it, and drove it with Playwright as
owner, member and second admin (list badges, live slug preview, read-only editor, admin save,
owner-only visibility, member losing sight after "Only me"). Screenshots were inspected; the
script is not committed.
