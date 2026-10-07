# Webhooks and the bookings API for integrations

How an external system (a CRM, a data warehouse) keeps an exact copy of Calnode's
bookings: webhooks for every change, and a read API to repair anything a webhook missed.

## Setup

1. **An org-wide webhook.** Admin → Webhooks → New webhook, tick *Every booking in the
   workspace*, pick the event types to send (none ticked = all), and copy the signing
   secret from the dialog. It is shown once. Or by API (an admin's key):

   ```
   POST /v1/webhooks
   {"url": "https://ops.example.com/webhooks/calnode",
    "events": ["booking.created","booking.rescheduled","booking.reassigned",
               "booking.cancelled","booking.updated","booking.rsvp"],
    "scope": "org",
    "event_types": ["demo"],
    "fields": [ ...every key you want, see "Payload" ... ]}
   ```

   The response carries `secret`. Omit `fields` for the default set, which leaves out
   personal data (names, emails, answers, hosts, attendees).
2. **A read-only key for reconciliation.** Admin → API Keys → New key, tick *Read
   bookings*. Or `POST /v1/api-keys {"name": "crm", "scopes": ["bookings:read"]}`. Made
   by an admin, it reads every booking in the workspace and can change nothing.

## Events

| Event | When | Extra fields |
|---|---|---|
| `booking.created` | A booking is made. Sent once the calendar has produced the meeting link. | |
| `booking.rescheduled` | The time moved. | `previous_start_at`, `previous_end_at` |
| `booking.reassigned` | The host changed, time unchanged. | `previous_host_id` (+ `previous_host_name`, `previous_host_email` if selected) |
| `booking.cancelled` | Cancelled. Final. | `cancellation_reason` |
| `booking.updated` | A carried field changed outside the events above, e.g. the calendar sweep minted the Meet link later. | `changed`: e.g. `["meeting","location_value"]` |
| `booking.rsvp` | The booker answered a Calnode-sent invite. | `rsvp_status` |
| `recording.completed`, `transcript.ready`, `notes.ready` | Calnode video (LiveKit) only. | |

`initiated_by` is on every change event: `booker` (through the booking's manage link, in
the confirmation email or the calendar invite; anyone the invite reached can follow it),
`host` (a host of the booking, signed in or by API key), `admin` (an admin who does not
host it, or a member's removal handing bookings on), `system` (Calnode itself).

## Delivery

```
POST <your url>
Content-Type: application/json
X-Calnode-Event: booking.created
X-Calnode-Delivery: <id, the same on every retry and redelivery of this delivery>
X-Calnode-Signature: sha256=<hex>[,sha256=<hex>]

{"event": "booking.created", "created_at": "<when queued>", "data": { ... }}
```

- **Answer 2xx** to accept. Anything else, or no answer in 10 s, is retried: 8 attempts,
  waiting 5 min, 15 min, 45 min, 2 h 15, then 6 h each, about 21 hours in all. After that
  the delivery is `failed` in the delivery log, where *Redeliver* (or
  `POST /v1/webhooks/{id}/deliveries/{delivery_id}/redeliver`) sends it again with the same
  delivery id and payload.
- **Dedupe on `X-Calnode-Delivery`.** A retry or a redelivery repeats it.
- **Order is not guaranteed** (retries reorder). Use `data.revision`: it goes up on every
  change to the booking, its hosts or its attendees. Apply an event's state only when its
  revision is higher than the one you hold; the payload's current-state fields (status,
  times, location, host, meeting) always match its revision. `occurred_at` is when the
  change happened, `created_at` (envelope) when the delivery was queued.

### Verifying the signature

HMAC-SHA256 of the **raw request body**, keyed by the secret **hex-decoded to its 32 raw
bytes**. Keying by the 64-character hex string is the common mistake and never verifies.
During a secret rotation the header holds two comma-separated values: accept the delivery
if either matches.

```js
import crypto from 'node:crypto';
function verify(rawBody, header, secretHex) {
  const key = Buffer.from(secretHex, 'hex');
  const want = 'sha256=' + crypto.createHmac('sha256', key).update(rawBody).digest('hex');
  return header.split(',').some((v) =>
    v.length === want.length && crypto.timingSafeEqual(Buffer.from(v), Buffer.from(want)));
}
```

No timestamp is signed; replay protection is the delivery id.

### Rotating the secret

`POST /v1/webhooks/{id}/rotate-secret` (or the key icon on the Webhooks page) returns a new
secret, once. For the next 24 hours every delivery is signed with both, so: rotate, put
the new secret in your receiver, done. The list shows `previous_secret_valid_until`.

## Payload

`data` holds the fields the webhook selected (`fields`; the UI ticks them all).

| Field | Default set | Notes |
|---|---|---|
| `id`, `status`, `start_at`, `end_at`, `created_at` | yes | RFC3339 UTC |
| `event_type_slug`, `host_id` | yes | `host_id` is the primary host |
| `location_value` | yes | the attendee's join link, or `tel:…` for a phone call |
| `meeting` | yes | see below |
| `revision`, `occurred_at`, `admin_url` | yes | `admin_url` opens the booking in Calnode's admin |
| `initiated_by`, `changed`, `previous_start_at`, `previous_end_at`, `previous_host_id`, `cancellation_reason`, `rsvp_status`, payment fields | yes | only when set |
| `event_type_name`, `host_name` | no | |
| `host_email`, `attendee_name`, `attendee_email`, `attendee_timezone`, `answers`, `hosts`, `attendees`, `previous_host_name`, `previous_host_email` | no | personal data |

```json
"meeting": {
  "provider": "google_meet",           // google_meet | teams | zoom | livekit | phone | in_person | manual
  "join_url": "https://meet.google.com/abc-defg-hij",
  "calendar_provider": "google",       // google | microsoft | caldav | none
  "calendar_event_id": "…",
  "ical_uid": "…@google.com"           // the primary host's event; note takers key on it
},
"hosts":     [{"id": "…", "name": "…", "email": "…", "role": "primary"}],   // primary | fixed | rotation | optional
"attendees": [{"name": "…", "email": "…", "timezone": "Europe/Berlin", "phone": "+49…",
               "locale": "de", "rsvp_status": "needs-action", "organizer": true}],
"answers":   [{"question_id": "…", "question": "Company", "answer": "…"}]
```

`manual` means a link the organizer typed, including a Meet or Teams event type whose host
had no matching calendar to generate one. For Calnode video the `join_url` is the
attendee's link; the host's link never leaves Calnode. The flat `host_*` and `attendee_*`
fields stay for existing receivers.

## Reading bookings

With the scoped key (`Authorization: Bearer cno_…`):

```
GET /v1/bookings?scope=all&updated_since=2026-10-07T00:00:00Z&include=hosts,attendees,answers,meeting&limit=200&offset=0
GET /v1/bookings/{id}
```

- `updated_since` returns every booking changed at or after it, **cancelled ones
  included**, ordered by `changed_at` (oldest first) for paging. Each item has `revision`
  and `changed_at`. Poll it (hourly, say) from the newest `changed_at` you hold to repair
  anything a lost webhook missed.
- `include` adds the same `hosts`, `attendees`, `answers` and `meeting` a webhook carries.
- `GET /v1/bookings/{id}` returns the full shape. It needs credentials (an admin, a host
  of the booking, or a scoped key) or the booking's manage token as `?token=`. It used to
  be public, and the booking id is printed on every calendar invite.
- A scoped key answers **403** on everything but the reads its scopes name:
  `bookings:read` (`GET /v1/bookings`, `GET /v1/bookings/{id}`, `GET /v1/bookings/{id}/answers`)
  and `webhooks:read` (`GET /v1/webhooks`, `GET /v1/webhooks/{id}/deliveries`).

The manage link (reschedule/cancel) is deliberately in no payload and no API response: it
is a bearer link, and whoever holds it can cancel the booking.
