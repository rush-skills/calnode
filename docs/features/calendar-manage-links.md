# Reschedule and cancel links on the calendar invite

When Calnode sends no email, the invite the host's calendar sends is the only message a
booker gets. Every booking's calendar event therefore carries two links, above the
Booking ID line:

```
Need to make a change?
Reschedule: https://bookings.example.com/manage/<token>?action=reschedule
Cancel: https://bookings.example.com/manage/<token>?action=cancel
```

Google and Microsoft get the HTML form (two named links); CalDAV gets the text above.

## How it works

- **One shared builder.** `calendarDescription` is the only place the event body is
  composed, and all four paths that create a booking's calendar event pass it links from
  `calendarManageLinks`: the inline create, the reconcile sweep, reassignment and member
  removal. They cannot drift.
- **The links open the existing manage page.** `?action=reschedule` opens the date and
  slot picker straight away; `?action=cancel` opens the cancel confirmation. The booker's
  name, email, answers and attendees are untouched by a reschedule: only the time moves.
  A GET never cancels anything, because mail and calendar scanners prefetch links; the
  booker still confirms on the page.
- **Calendar tokens are never rotated.** A reschedule rotates the manage token behind
  the confirmation-email link, so a forwarded old email stops working. The calendar
  event's description is not rewritten on a reschedule, so its token
  (`purpose = 'calendar'`) is left alone and its expiry is pushed out to cover the new
  time. Reassignment and the reconcile sweep recreate the event, so they mint a fresh one.
- **Tokens outlive the meeting.** Every manage token, email or calendar, now expires 60
  days after issue or a week after the booking ends, whichever is later. Before, a
  booking made more than 60 days ahead had a dead link before the meeting.
- **No links without a public URL.** A calendar client cannot follow a relative link, so
  nothing is written, and no token is minted, unless `BASE_URL` is an absolute
  `http(s)://` URL.

## Cancelled meetings drop the links

Google's cancellation email quotes the event's description as it stands when the event
is deleted. So before any booking event is deleted (a cancel, the reconcile sweep, a
reassignment, a member's removal), `dropCalendarManageLinks` rewrites the description
without the links, with `sendUpdates=none` so that rewrite emails nobody. The
cancellation email then shows the message, answers and Booking ID, but no links.

This runs on Google only (`calendar.DescriptionSetter`). Microsoft Graph emails
attendees about an organizer's edit, which would add a second email before the
cancellation, so Microsoft and CalDAV events are deleted as they are. A link followed
after cancellation lands on the manage page's "cancelled" state, which offers nothing.

## Who can use the links

Everyone the calendar event is shared with: the booker, every host on the booking, and
the workspace's default participants. That is the audience that can already see the
meeting, and the page only offers what the booker could do anyway. A cancel or
reschedule made through a link is reported to webhooks as `initiated_by: "booker"`.

## Existing bookings

Events created before this change keep their old description. Their bookings can still
be managed from the confirmation email, if one was sent, or by a host in the admin app.

## Telling other systems

`booking.cancelled` and `booking.rescheduled` fire for every cancel and reschedule, from
the manage page as well as the admin app and API. Each now carries `initiated_by`
(`booker` or `host`). For an integration that should hear about the whole team rather
than one host's bookings, an admin creates the webhook with **Every booking in the
workspace** ticked (`"scope": "org"` in `POST /v1/webhooks`).
