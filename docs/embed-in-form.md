# Booking a meeting inside your own form

Your site already has a lead or contact form. This page shows how to put a Calnode
time picker **inside that form**, so the visitor fills in one form, presses one submit
button, and the meeting is booked together with the lead.

There are two ways to do it:

| | Embed picker (recommended) | Native picker (JSON API) |
|---|---|---|
| What you write | One HTML element plus a few lines of JS | Your own calendar UI |
| Translations, timezones, availability rules | Handled by the widget | You handle them |
| Fits the form | Real form field: `required`, reset and `FormData` all work | Whatever you build |
| Who creates the booking | Browser (`picker.book()`) or your backend | Browser or your backend |

Start with the embed picker. Build a native picker only if the widget cannot match your
design.

Every example uses the same event type: **slug `intro-call`, 30 minutes**, on a
Calnode instance at `https://booking.example.com`. Replace both with your own.

Working demo: [`embed-form-example.html`](embed-form-example.html) (recipe A, with
name, email and company fields, a compact picker, and one submit button).

---

## How it works

1. `<calnode-booking mode="picker">` renders only the calendar and the time slots. It
   does not show a details form or a confirmation screen, and **picking a time does not
   book it**.
2. The element is a form field. When your form is submitted, it sends the chosen slot
   as `meeting=2026-10-07T14:00:00Z` (in UTC), plus `meeting_end` and
   `meeting_timezone`.
3. Something then creates the booking with `POST /v1/bookings`. That is either the
   browser, through `picker.book()` in your submit handler (recipe A), or your own
   backend after it receives the form (recipe B).

If someone else books the same time first, the booking call returns `409`. In the
browser, `book()` rejects with `err.code === 'slot_taken'`, clears the selection and
reloads the times, so the visitor can pick another time and submit again.

---

## Recipe A: picker, then book from the browser in the same submit handler

This is the simplest recipe. Calnode sends the confirmation email and calendar invite.
Your backend receives the lead along with the booking id.

```html
<script src="https://booking.example.com/embed.js" async></script>

<form id="lead" novalidate>
  <label>Name <input name="name" autocomplete="name" required></label>
  <label>Work email <input name="email" type="email" autocomplete="email" required></label>
  <label>Company <input name="company" autocomplete="organization"></label>

  <div id="meeting-label">Meeting time</div>
  <calnode-booking id="picker" slug="intro-call" mode="picker" compact required
                   name="meeting" aria-labelledby="meeting-label"></calnode-booking>

  <p id="error" role="alert"></p>
  <button type="submit" id="submit">Request my call</button>
</form>

<script>
  const form = document.getElementById('lead');
  const picker = document.getElementById('picker');
  const errorEl = document.getElementById('error');
  const submit = document.getElementById('submit');

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    errorEl.textContent = '';
    // Checks your own fields AND the picker (it is `required`).
    if (!form.reportValidity()) return;

    const data = new FormData(form); // name, email, company, meeting, meeting_end, meeting_timezone
    submit.disabled = true;
    try {
      // 1. Book the meeting. This uses the selected slot and the visitor's timezone.
      const booking = await picker.book({
        name: data.get('name'),
        email: data.get('email'),
        // answers: { '<question-id>': 'value' },   // if the event type has intake questions
      });

      // 2. Send the lead to your own backend, with the booking id to join the two later.
      const lead = Object.fromEntries(data);
      lead.booking_id = booking.id;
      await fetch('/api/leads', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(lead),
      });

      form.replaceWith(Object.assign(document.createElement('p'), {
        textContent: 'Thanks! Your call is booked. Check your inbox for the invite.',
      }));
    } catch (err) {
      if (err.code === 'slot_taken') {
        // The picker has already cleared the choice and reloaded the times.
        errorEl.textContent = 'That time was just taken. Please pick another one.';
      } else {
        // 'invalid' (e.g. bad email, missing required answer, booking limit reached),
        // 'rate_limited', 'failed', 'network': err.message is safe to show.
        errorEl.textContent = err.message;
      }
    } finally {
      submit.disabled = false;
    }
  });
</script>
```

Notes:

- Book first, then save the lead. If the booking fails, the visitor is still on the
  page to fix it. If you would rather never lose a lead, save the lead first and
  update it with the booking id afterwards.
- `book()` also fires `calnode:booked` (the same event the full widget fires).
- **Paid event types:** `book()` resolves with
  `{payment_required: true, booking_id, checkout_url}` and nothing else. Redirect the
  visitor to `checkout_url` to pay. The slot is held for the booking until then.

---

## Recipe B: picker as a form field, your backend creates the booking

Use this when your backend already processes the form, for example to enrich it or
push it to a CRM, and you want the booking created there. The page has no booking
code at all: the picker is just a field.

```html
<script src="https://booking.example.com/embed.js" async></script>

<form method="post" action="/api/leads">
  <input name="name" required>
  <input name="email" type="email" required>
  <input name="company">
  <calnode-booking slug="intro-call" mode="picker" compact required name="meeting"></calnode-booking>
  <button type="submit">Request my call</button>
</form>
```

Your backend receives:

```
name=Grace Hopper
email=grace@example.com
company=Acme
meeting=2026-10-07T14:00:00Z
meeting_end=2026-10-07T14:30:00Z
meeting_timezone=Europe/Berlin
```

Then it creates the booking:

```bash
curl -sS https://booking.example.com/v1/bookings \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: lead-8c1f2a' \
  -d '{
    "event_type_slug": "intro-call",
    "start_at": "2026-10-07T14:00:00Z",
    "name": "Grace Hopper",
    "email": "grace@example.com",
    "timezone": "Europe/Berlin",
    "language": "en",
    "answers": []
  }'
```

The same call in Node 18+ (`fetch` is built in):

```js
// POST /api/leads handler (Express shown; any framework works)
app.post('/api/leads', express.urlencoded({ extended: false }), async (req, res) => {
  const { name, email, company, meeting, meeting_timezone } = req.body;

  const r = await fetch('https://booking.example.com/v1/bookings', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      // Optional. Makes a retry after a timeout safe: the same key replays the
      // original response instead of creating a second booking.
      'Idempotency-Key': `lead-${email}-${meeting}`,
    },
    body: JSON.stringify({
      event_type_slug: 'intro-call',
      start_at: meeting,
      name,
      email,
      timezone: meeting_timezone || 'UTC',
    }),
  });
  const body = await r.json();

  if (r.status === 409) {
    // Someone took the slot between the visitor picking it and this request.
    return res.status(409).send('That time was just taken. Please go back and pick another.');
  }
  if (!r.ok) return res.status(400).send(body.error || 'Could not book the meeting');

  await saveLead({ name, email, company, bookingId: body.id, meetingStart: body.start_at });
  res.redirect(303, '/thanks');
});
```

Things to know for server-side booking:

- **`POST /v1/bookings` is public.** It takes no API key. Sending `X-API-Key` or
  `Authorization` does nothing, because the endpoint does not read them. Treat the
  form's backend like any other client.
- **Rate limits apply to your backend's IP.** The limit is 20 booking requests per
  minute per IP, and every lead your server books counts against the same IP. If
  your server is a reverse proxy in front of the visitors, add it to Calnode's
  `TRUSTED_PROXY_CIDRS` and forward `X-Forwarded-For`. The limit then applies per
  visitor again.
- Also enforced: at most **10 bookings per email address per rolling hour** (`429`),
  and the event type's *max active bookings per invitee* (`422`).
- Calnode sends the confirmation email and invite to `email`, exactly as for any
  other booking.
- Validation still happens at commit time. The slot is rechecked against availability
  and the hosts' calendars, so a stale or forged `start_at` gets a `409`, not a
  booking.

---

## Recipe C: native picker via the JSON API

Build your own calendar UI against the same public endpoints the widget uses. They
are all CORS-enabled for browsers (`Access-Control-Allow-Origin: *` by default, or only
the origins listed in `EMBED_ALLOWED_ORIGINS`) and need no authentication.

```html
<form id="lead">
  <input name="name" required>
  <input name="email" type="email" required>
  <select name="meeting" id="meeting" required><option value="">Loading times...</option></select>
  <button type="submit">Request my call</button>
  <p id="error" role="alert"></p>
</form>

<script>
  const BASE = 'https://booking.example.com';
  const SLUG = 'intro-call';
  const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const select = document.getElementById('meeting');

  const ymd = (d) => d.toISOString().slice(0, 10);

  async function loadSlots() {
    // 1. Public info: name, duration, locale, accent colour, max_future_days ...
    const info = await fetch(`${BASE}/v1/event-types/${SLUG}/public`).then((r) => r.json());

    // 2. Slots for the next 14 days, rendered in the visitor's timezone.
    const from = new Date();
    const to = new Date(Date.now() + 14 * 864e5);
    const res = await fetch(
      `${BASE}/v1/event-types/${SLUG}/slots?from=${ymd(from)}&to=${ymd(to)}&tz=${encodeURIComponent(TZ)}`
    ).then((r) => r.json());

    // 3. Render. Each slot is {start, end, host_ids}; start/end carry the TZ offset.
    const fmt = new Intl.DateTimeFormat(info.locale, {
      timeZone: TZ, weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
    });
    select.innerHTML = '<option value="">Choose a time</option>';
    for (const s of res.slots) {
      select.add(new Option(`${fmt.format(new Date(s.start))} (${info.duration_label})`, s.start));
    }
  }

  document.getElementById('lead').addEventListener('submit', async (e) => {
    e.preventDefault();
    const data = new FormData(e.target);
    // 4. Book.
    const r = await fetch(`${BASE}/v1/bookings`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        event_type_slug: SLUG,
        start_at: data.get('meeting'),
        name: data.get('name'),
        email: data.get('email'),
        timezone: TZ,
      }),
    });
    const body = await r.json();
    if (r.status === 409) { document.getElementById('error').textContent = 'That time was just taken.'; return loadSlots(); }
    if (!r.ok) { document.getElementById('error').textContent = body.error; return; }
    // body.id is the booking id; now submit the lead to your backend.
  });

  loadSlots();
</script>
```

A real picker should group slots by day (`new Date(s.start)` formatted in `TZ`) and
page through months instead of using one `<select>`. Request at most about a month per
`/slots` call: the endpoint is rate limited (60/min per IP) and each call checks the
hosts' calendars.

---

## Reference: `<calnode-booking>` in picker mode

Load the script once per page: `<script src="https://<your-instance>/embed.js" async></script>`.
The widget calls the instance that served the script.

### Attributes

| Attribute | Default | Meaning |
|---|---|---|
| `slug` | (required) | Event type slug, e.g. `intro-call`. |
| `mode` | full booking flow | `picker`: calendar and slots only. Selecting a time does not book it. |
| `compact` | off | Smaller layout for forms: no event header, host, description or chat link. Calendar stacked above the slots, tighter spacing, smaller type, bordered instead of shadowed. Works from about 300px wide. Opens on the first day that has free times. Without `compact`, picker mode keeps the event info pane and the responsive one-step-at-a-time layout on narrow screens. |
| `required` | off | Blocks form submit until a time is picked (`valueMissing`, with a translated message). Can be toggled at runtime. |
| `name` | `calnode_slot` | Form field name. Also prefixes the companion fields `<name>_end` and `<name>_timezone`. |
| `show-timezone` | `true` | `"false"` hides the "Times shown in Europe/Berlin" line and its switcher. Leave it on unless your form already says which timezone the times are in. |
| `timezone` | browser's zone | IANA zone to show the times in, e.g. `Asia/Kolkata`. An unknown value is ignored. Can be changed at runtime: the month reloads in the new zone. |
| `lang` | auto | Force a language (`es`, `fr`, ...). Otherwise it comes from the browser's `Accept-Language`. |

Colours follow the instance's **booking accent** (Settings > Branding). The selected
day and time use it.

**Timezone.** The times start in the visitor's browser timezone. The zone in the "Times
shown in" line is a button: it opens a search over every IANA zone the browser knows. A
zone is found by its name or city (`kolkata`, `new york`), its English name
(`india standard time`), a common abbreviation (`IST`, `PST`, `CEST`) or its offset
(`+5:30`, `GMT+5:30`). A change reloads the month in that zone. A selected time stays
selected: the instant does not change, only its labels and `meeting_timezone`.

Attributes other than `required` are read once, when the element connects. To change
`slug` or `mode`, replace the element.

### Submitted form fields

When a time is selected (shown here with `name="meeting"`):

| Field | Example | |
|---|---|---|
| `meeting` | `2026-10-07T14:00:00Z` | Slot start, RFC 3339, UTC. Send this as `start_at`. |
| `meeting_end` | `2026-10-07T14:30:00Z` | Slot end, UTC. |
| `meeting_timezone` | `Europe/Berlin` | The visitor's IANA timezone. Send this as `timezone` so their emails use it. |

With nothing selected, none of the fields are submitted. In browsers without
`ElementInternals` (older Safari), the widget writes the same three fields as hidden
inputs inside the element, and a capture-phase submit listener enforces `required`.

### Properties and methods

| Member | Returns | |
|---|---|---|
| `value` | `string` | Selected start (UTC RFC 3339) or `''`. Setting `''` clears the selection. |
| `selectedSlot` | `object \| null` | `{start, end, timezone, event_type, host_ids}`, a copy. |
| `reset()` | | Clears the selection (fires `calnode:slot-cleared`). Also runs on the form's `reset`. |
| `refresh()` | `Promise<void>` | Reloads the visible month's times. Drops the selection if that time is gone. |
| `book(details)` | `Promise<object>` | Books the selected slot. See below. |
| `form`, `validity`, `validationMessage`, `checkValidity()`, `reportValidity()` | | Standard form-field members. |

`book(details)` sends `POST /v1/bookings` with the selected slot. `details`:

| Key | | |
|---|---|---|
| `name` | required | Booker's name. |
| `email` | required | Booker's email. Confirmation and invite go here. |
| `phone` | optional | Only when the event type allows a phone call. |
| `answers` | optional | Intake answers, either `[{question_id, value}]` or `{ "<question_id>": "value" }`. Checkboxes take `"yes"` or `"no"`. |
| `language` | optional | Locale for emails. Defaults to the widget's resolved language. |
| `timezone` | optional | Defaults to the visitor's browser timezone. |
| `idempotencyKey` | optional | Sent as `Idempotency-Key`, for safe retries. |

There is no free-text `notes` field on a booking. To collect notes, add a text intake
question to the event type and send it in `answers`.

It resolves with the booking JSON (see `POST /v1/bookings` below), or, for paid event
types, `{payment_required, booking_id, checkout_url}`. Calling it again while a call
is in flight returns the same promise. It rejects with an `Error` whose `.code` is:

| `.code` | When | Widget behaviour |
|---|---|---|
| `no_slot` | Nothing selected | (none) |
| `slot_taken` | `409`: the time is no longer free | Clears the selection, reloads the times, shows a translated notice |
| `invalid` | `400`/`422`: bad email, missing required answer, booking limit reached ... | `.message` is the server's message (translated where a visitor can hit it) |
| `rate_limited` | `429` | (none) |
| `failed` | Any other non-2xx response | (none) |
| `network` | The request never got a response | (none) |

`.status` holds the HTTP status when there was a response.

### Events

All of these bubble and are `composed`, so you can listen on the element, the form or
`document`.

| Event | `detail` |
|---|---|
| `calnode:slot-selected` | `{start, end, timezone, event_type, host_ids}`. Times are UTC RFC 3339. |
| `calnode:slot-cleared` | `{event_type}` |
| `calnode:timezone-changed` | `{timezone, event_type}`. After it, `calnode:slot-selected` fires again when a time is selected, with the new `timezone`. |
| `calnode:booked` | The booking JSON. Fired by `book()`, and by the normal booking flow. |
| `change` | Plain `Event`, fired on select and on clear (for framework bindings). |

---

## Reference: the public JSON API

All endpoints are unauthenticated and CORS-enabled. Errors always have the shape
`{"error": "message"}`.

### `GET /v1/event-types/{slug}/public`

Display info. Optional `?lang=es` forces a language. Otherwise the language comes from
`Accept-Language`.

```json
{
  "slug": "intro-call",
  "name": "Intro call",
  "description": "A quick 30-minute intro.",
  "duration_minutes": 30,
  "duration_label": "30 min",
  "location_type": "in_person",
  "location_label": "In Person",
  "allow_phone_call": false,
  "booking_accent": "#111827",
  "booking_accent_foreground": "#ffffff",
  "max_future_days": 60,
  "min_notice_minutes": 0,
  "min_notice_label": "",
  "assistant_enabled": false,
  "assistant_greeting": "Hi! ...",
  "price_cents": 0,
  "currency": "usd",
  "hosts": [{ "name": "Ada Host", "avatar_url": "https://booking.example.com/..." }],
  "business_name": "",
  "logo_url": "",
  "banner_url": "",
  "locale": "en",
  "i18n": { "select_day_hint": "Select a day to see available times.", "...": "..." }
}
```

`hosts` is empty when the workspace hides host names. `404` if the event type is not
active and public.

### `GET /v1/event-types/{slug}/slots?from=YYYY-MM-DD&to=YYYY-MM-DD&tz=Area/City`

Bookable times. `from` defaults to today and `to` to the event type's booking horizon,
which also caps it. `tz` defaults to `UTC` and controls the offset of the returned
times and which calendar day `from`/`to` mean.

```json
{
  "slots": [
    {
      "start": "2026-10-07T11:00:00+02:00",
      "end": "2026-10-07T11:30:00+02:00",
      "host_ids": ["eb391edb-f3b6-47fc-9b5a-3aa46ef31d5a"]
    }
  ],
  "hosts": {
    "eb391edb-f3b6-47fc-9b5a-3aa46ef31d5a": { "name": "Ada Host", "avatar_url": "" }
  }
}
```

Optional keys, each present only when it applies:

- `"taken": [{start, end}]`: already-booked times, only for event types with *Show
  taken slots* on. These are never bookable.
- `"min_notice": {"minutes": 240, "dates": ["2026-10-07"]}`: days on which the
  minimum-notice rule removed times.
- `"degraded": true`: a host calendar could not be checked, so some offered times may
  be rejected at booking.

`start` can be sent to `POST /v1/bookings` as is. Any RFC 3339 offset is accepted.
Errors: `400` for a bad `tz` or date range, `404` for an unknown slug, `429` above 60
requests per minute per IP.

### `GET /v1/event-types/{slug}/questions`

Intake questions to render in your form:

```json
{ "items": [
  { "id": "q_123", "event_type_id": "...", "label": "What should we cover?", "type": "text", "required": true, "position": 0 },
  { "id": "q_456", "event_type_id": "...", "label": "Team size", "type": "select", "options": ["1-10", "11-50", "50+"], "required": false, "position": 1 }
] }
```

`type` is `text`, `select` or `checkbox`.

### `POST /v1/bookings`

Request:

```json
{
  "event_type_slug": "intro-call",
  "start_at": "2026-10-07T14:00:00Z",
  "name": "Grace Hopper",
  "email": "grace@example.com",
  "timezone": "Europe/Berlin",
  "language": "en",
  "phone": "",
  "answers": [{ "question_id": "q_123", "value": "Pricing" }]
}
```

`event_type_slug`, `start_at`, `name` and `email` are required. `timezone` defaults
to `UTC`, and `language` to English. Optional header: `Idempotency-Key`. The end time
comes from the event type's duration.

Response `201`:

```json
{
  "id": "966207e7-fc23-4c16-9be4-74eace36a905",
  "event_type_id": "671770b4-1dcc-4c5d-b38e-d749032552f7",
  "host_id": "eb391edb-f3b6-47fc-9b5a-3aa46ef31d5a",
  "start_at": "2026-10-07T14:00:00Z",
  "end_at": "2026-10-07T14:30:00Z",
  "status": "confirmed",
  "location_type": "in_person",
  "created_at": "2026-10-06T06:40:26Z",
  "updated_at": "2026-10-06T06:40:26Z",
  "hosts": [{ "id": "eb391edb-f3b6-47fc-9b5a-3aa46ef31d5a", "name": "Ada Host" }]
}
```

Paid event type: `200` with `{"payment_required": true, "booking_id": "...", "checkout_url": "https://checkout.stripe.com/..."}`.

| Status | Body `error` (examples) | Meaning |
|---|---|---|
| `400` | `event_type_slug, start_at, name, and email are required`, `start_at must be RFC3339 ...`, `invalid telephone number`, intake validation messages | Fix the request |
| `404` | `event type not found` | Unknown, inactive or private slug |
| `409` | `this slot is no longer available` | Taken, outside availability, or blocked by a calendar. Pick again. |
| `422` | booking limit reached for this email | The event type's per-invitee limit |
| `429` | `rate limit exceeded` / per-email throttle | 20/min per IP, 10/hour per email |
| `503` | calendar or payments unavailable | Retry later |

---

## Testing locally

1. Run Calnode, e.g. `PORT=3000 BASE_URL=http://localhost:3000 go run ./cmd/calnode`,
   and create a public event type with the slug `intro-call` and some availability.
2. Serve the docs folder from a **different origin**:
   `python3 -m http.server 8099 --directory docs`.
3. Open `http://localhost:8099/embed-form-example.html?base=http://localhost:3000`.
   You can also edit `data-base` on the page's `<html>` element. `?slug=` picks a
   different event type.
