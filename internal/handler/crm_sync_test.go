package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/webhook"
)

// End-to-end checks for the CRM-sync webhook and API work (docs/webhooks.md).

// meetCalendar mints a Meet link and reports an iCalUID, as Google does.
type meetCalendar struct{ linkCalendar }

func (p meetCalendar) CreateEvent(_ context.Context, _ string, in calendar.CreateEventParams) (string, string, string, error) {
	if in.ICalUID != nil {
		*in.ICalUID = "evt-1@google.com"
	}
	p.events <- in
	return "evt-1", "https://meet.google.com/abc-defg-hij", "primary", nil
}

// addOrgWebhook registers an org-wide webhook that sends every field for these events.
func addOrgWebhook(t *testing.T, db *sql.DB, ownerID string, events ...string) {
	t.Helper()
	ev, _ := json.Marshal(events)
	fields, _ := json.Marshal(webhook.AllFields)
	mustExec(t, db, `INSERT INTO webhooks (id, user_id, url, events, fields, secret_enc, scope) VALUES (?, ?, 'https://hooks.example.com/x', ?, ?, 'x', 'org')`,
		"wh-"+events[0], ownerID, string(ev), string(fields))
}

type deliveredBooking struct {
	Event string `json:"event"`
	Data  struct {
		ID             string `json:"id"`
		Location       string `json:"location_value"`
		InitiatedBy    string `json:"initiated_by"`
		Revision       int    `json:"revision"`
		OccurredAt     string `json:"occurred_at"`
		AdminURL       string `json:"admin_url"`
		HostID         string `json:"host_id"`
		PreviousHostID string `json:"previous_host_id"`
		Meeting        struct {
			Provider string `json:"provider"`
			JoinURL  string `json:"join_url"`
			ICalUID  string `json:"ical_uid"`
		} `json:"meeting"`
		Hosts []struct {
			ID, Email, Role string
		} `json:"hosts"`
		Attendees []struct {
			Email     string `json:"email"`
			Organizer bool   `json:"organizer"`
		} `json:"attendees"`
	} `json:"data"`
}

// waitDelivery waits for the nth (1-based) delivery of event and decodes it.
func waitDelivery(t *testing.T, db *sql.DB, event string, n int) deliveredBooking {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := db.Query(`SELECT payload FROM webhook_deliveries WHERE event = ? ORDER BY rowid`, event)
		if err != nil {
			t.Fatal(err)
		}
		var payloads []string
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			payloads = append(payloads, p)
		}
		rows.Close()
		if len(payloads) >= n {
			var d deliveredBooking
			if err := json.Unmarshal([]byte(payloads[n-1]), &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s delivery #%d never queued (have %d)", event, n, len(payloads))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// PRD acceptance 1 and 2: booking.created carries the Meet link the calendar minted
// (it used to carry the event type's static location), with the meeting object, the
// iCalUID, every host and attendee, and a revision that a later reschedule raises.
// A host's reschedule reports "host", another admin's "admin".
func TestCRMSync_createdCarriesMeetLinkAndRescheduleActors(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	h.SetBaseURL("https://book.example.com")
	slug, etID := seedEventTypeHTTP(t, h, key)
	mustExec(t, db, `UPDATE event_types SET location_type = 'google_meet', location_value = '' WHERE id = ?`, etID)
	mustExec(t, db, `INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, ownerID)
	p := meetCalendar{linkCalendar{telephoneCalendar{events: make(chan calendar.CreateEventParams, 4)}}}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)
	addOrgWebhook(t, db, ownerID, "booking.created", "booking.rescheduled")

	start := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Hour)
	bookingID := createBookingViaHTTP(t, h, slug, start.Format(time.RFC3339))

	created := waitDelivery(t, db, "booking.created", 1)
	d := created.Data
	if d.Location != "https://meet.google.com/abc-defg-hij" || d.Meeting.JoinURL != d.Location || d.Meeting.Provider != "google_meet" {
		t.Errorf("booking.created location %q meeting %+v; want the minted Meet link", d.Location, d.Meeting)
	}
	if d.Meeting.ICalUID != "evt-1@google.com" {
		t.Errorf("ical_uid = %q", d.Meeting.ICalUID)
	}
	if len(d.Hosts) != 1 || d.Hosts[0].Role != "primary" || len(d.Attendees) != 1 || !d.Attendees[0].Organizer {
		t.Errorf("hosts %+v attendees %+v", d.Hosts, d.Attendees)
	}
	if d.Revision < 1 || d.OccurredAt == "" || d.AdminURL != "https://book.example.com/admin/bookings/"+bookingID {
		t.Errorf("revision %d occurred_at %q admin_url %q", d.Revision, d.OccurredAt, d.AdminURL)
	}

	reschedule := func(apiKey string, at time.Time) {
		t.Helper()
		req := authReq(http.MethodPost, "/v1/bookings/"+bookingID+"/reschedule",
			fmt.Sprintf(`{"start_at":%q}`, at.Format(time.RFC3339)), apiKey)
		req.SetPathValue("id", bookingID)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.RescheduleBooking)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("reschedule: %d %s", rec.Code, rec.Body)
		}
	}
	reschedule(key, start.Add(time.Hour))
	byHost := waitDelivery(t, db, "booking.rescheduled", 1)
	if byHost.Data.InitiatedBy != "host" {
		t.Errorf("host reschedule initiated_by = %q; want host", byHost.Data.InitiatedBy)
	}
	if byHost.Data.Revision <= d.Revision {
		t.Errorf("revision %d after reschedule; want more than %d", byHost.Data.Revision, d.Revision)
	}

	adminKey := seedMemberKey(t, db, "a2", "admin2@example.com")
	mustExec(t, db, `UPDATE users SET is_admin = 1 WHERE id = 'a2'`)
	reschedule(adminKey, start.Add(2*time.Hour))
	if got := waitDelivery(t, db, "booking.rescheduled", 2).Data.InitiatedBy; got != "admin" {
		t.Errorf("admin reschedule initiated_by = %q; want admin", got)
	}
}

// PRD P1.4: reassigning a booking is its own event, naming the previous host.
func TestCRMSync_reassignIsItsOwnEvent(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	slug, etID := seedEventTypeHTTP(t, h, key)
	seedMember(t, db, "u2", "second@example.com")
	seedFullAvailabilityDB(t, db, "u2")
	mustExec(t, db, `INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority) VALUES ('eh2', ?, 'u2', 'rotation', 1)`, etID)
	addOrgWebhook(t, db, ownerID, "booking.reassigned")
	bookingID := createBookingViaHTTP(t, h, slug, time.Now().UTC().Add(72*time.Hour).Truncate(time.Hour).Format(time.RFC3339))

	req := authReq(http.MethodPost, "/v1/bookings/"+bookingID+"/reassign", `{"host_id":"u2"}`, key)
	req.SetPathValue("id", bookingID)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign: %d %s", rec.Code, rec.Body)
	}
	d := waitDelivery(t, db, "booking.reassigned", 1).Data
	if d.HostID != "u2" || d.PreviousHostID != ownerID || d.InitiatedBy != "host" {
		t.Errorf("host %q previous %q by %q; want u2, %s, host", d.HostID, d.PreviousHostID, d.InitiatedBy, ownerID)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE event = 'booking.rescheduled'`).Scan(&n)
	if n != 0 {
		t.Errorf("reassignment also sent %d booking.rescheduled; want none", n)
	}
}

// PRD P1.7 and acceptance 4: a scoped key reads every booking, changes included
// (updated_since, cancelled ones too), and is refused any write.
func TestCRMSync_scopedKeyReadsAndCannotWrite(t *testing.T) {
	h, db, ownerKey, _ := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, ownerKey)
	since := time.Now().UTC().Add(-time.Minute)
	keep := createBookingViaHTTP(t, h, slug, time.Now().UTC().Add(72*time.Hour).Truncate(time.Hour).Format(time.RFC3339))
	gone := createBookingViaHTTP(t, h, slug, time.Now().UTC().Add(96*time.Hour).Truncate(time.Hour).Format(time.RFC3339))
	mustExec(t, db, `UPDATE bookings SET status = 'cancelled' WHERE id = ?`, gone)

	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateAPIKey)(rec, authReq(http.MethodPost, "/v1/api-keys", `{"name":"os","scopes":["bookings:read"]}`, ownerKey))
	scoped := mustString(t, mustCreated(t, rec, "create scoped key"), "key", "create scoped key")

	call := func(method, pattern, url string, fn http.HandlerFunc) *httptest.ResponseRecorder {
		req := authReq(method, url, "", scoped)
		req.Pattern = pattern
		rec := httptest.NewRecorder()
		h.RequireAuth(fn)(rec, req)
		return rec
	}
	rec = call(http.MethodGet, "GET /v1/bookings", "/v1/bookings?scope=all&updated_since="+since.Format(time.RFC3339)+"&include=hosts,attendees,answers,meeting", h.ListBookings)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []struct {
			ID       string          `json:"id"`
			Status   string          `json:"status"`
			Revision int             `json:"revision"`
			Meeting  json.RawMessage `json:"meeting"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	seen := map[string]string{}
	for _, it := range list.Items {
		seen[it.ID] = it.Status
		if it.Revision < 1 || len(it.Meeting) == 0 {
			t.Errorf("item %s lacks revision or meeting", it.ID)
		}
	}
	if seen[keep] != "confirmed" || seen[gone] != "cancelled" {
		t.Errorf("updated_since list = %v; want both bookings, the cancelled one included", seen)
	}

	rec = call(http.MethodPost, "POST /v1/bookings/{id}/cancel", "/v1/bookings/"+keep+"/cancel", h.CancelBooking)
	if rec.Code != http.StatusForbidden {
		t.Errorf("scoped key cancel: %d; want 403", rec.Code)
	}
	rec = call(http.MethodPost, "POST /v1/api-keys", "/v1/api-keys", h.CreateAPIKey)
	if rec.Code != http.StatusForbidden {
		t.Errorf("scoped key minting a key: %d; want 403", rec.Code)
	}
	if rec := call(http.MethodPost, "POST /v1/api-keys", "/v1/api-keys", h.CreateAPIKey); !strings.Contains(rec.Body.String(), "scopes") {
		t.Errorf("403 body should say why: %s", rec.Body)
	}
}

// PRD P0.4: a webhook made through the API returns its secret once, and a rotation
// returns a new one while the old keeps signing for 24 hours.
func TestCRMSync_rotateSecret(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateWebhook)(rec, authReq(http.MethodPost, "/v1/webhooks",
		`{"url":"https://example.com/hook","events":["booking.created"],"event_types":["demo"]}`, key))
	created := mustCreated(t, rec, "create webhook")
	id := mustString(t, created, "id", "create webhook")
	first := mustString(t, created, "secret", "create webhook")
	if types, _ := created["event_types"].([]any); len(types) != 1 || types[0] != "demo" {
		t.Errorf("event_types = %v; want [demo]", created["event_types"])
	}

	req := authReq(http.MethodPost, "/v1/webhooks/"+id+"/rotate-secret", "", key)
	req.SetPathValue("id", id)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.RotateWebhookSecret)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	var rot map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rot)
	if s, _ := rot["secret"].(string); len(s) != 64 || s == first {
		t.Errorf("rotated secret %q; want a new 64-hex secret", rot["secret"])
	}
	until, _ := time.Parse(time.RFC3339, fmt.Sprint(rot["previous_secret_valid_until"]))
	if d := time.Until(until); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("old secret valid for %v; want about 24h", d)
	}
}
