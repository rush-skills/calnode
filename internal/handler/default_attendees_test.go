package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/slots"
)

// attendeeCalendar is a provider that hands every CreateEvent's params to the test, so it can
// assert what the host's calendar event would carry. Reused across the booking and reassign
// paths; the reconcile path has its own internal-package sibling.
type attendeeCalendar struct {
	calendar.Provider
	created chan calendar.CreateEventParams
}

func (p attendeeCalendar) Name() string                                         { return "google" }
func (p attendeeCalendar) InvitesGuests() bool                                  { return true }
func (p attendeeCalendar) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p attendeeCalendar) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p attendeeCalendar) CreateEvent(_ context.Context, _ string, params calendar.CreateEventParams) (string, string, string, error) {
	p.created <- params
	return "event-id", "", "primary", nil
}
func (p attendeeCalendar) CancelEvent(context.Context, string, string, string) error { return nil }

func awaitCreate(t *testing.T, p attendeeCalendar) calendar.CreateEventParams {
	t.Helper()
	select {
	case params := <-p.created:
		return params
	case <-time.After(5 * time.Second):
		t.Fatal("calendar event was not created")
		return calendar.CreateEventParams{}
	}
}

func participantsGet(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	GetParticipantSettings(http.ResponseWriter, *http.Request)
}, key string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetParticipantSettings)(rec, authReq(http.MethodGet, "/v1/settings/participants", "", key))
	body := mustJSON(t, rec, http.StatusOK, "get participants")
	raw, ok := body["default_attendee_emails"].([]any)
	if !ok {
		t.Fatalf("default_attendee_emails missing or not a list: %v", body)
	}
	out := []string{}
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func participantsPatch(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	PatchParticipantSettings(http.ResponseWriter, *http.Request)
}, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchParticipantSettings)(rec, authReq(http.MethodPatch, "/v1/settings/participants", body, key))
	return rec
}

func TestParticipantSettings_roundTrip(t *testing.T) {
	h, key, _ := setupWorkspace(t)

	if got := participantsGet(t, h, key); len(got) != 0 {
		t.Fatalf("fresh workspace has default participants %v; want none", got)
	}

	// Set: normalised (trimmed, lower-cased, deduped) and echoed back in order.
	rec := participantsPatch(t, h, `{"default_attendee_emails":[" Notes@Example.com ","bot@example.com","notes@example.com",""]}`, key)
	body := mustJSON(t, rec, http.StatusOK, "patch participants")
	want := []any{"notes@example.com", "bot@example.com"}
	if got := body["default_attendee_emails"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("patch echoed %v; want %v", got, want)
	}
	if got := participantsGet(t, h, key); !reflect.DeepEqual(got, []string{"notes@example.com", "bot@example.com"}) {
		t.Fatalf("stored %v; want notes + bot", got)
	}

	// Omitted keeps.
	mustStatus(t, participantsPatch(t, h, `{}`, key), http.StatusOK, "patch nothing")
	if got := participantsGet(t, h, key); len(got) != 2 {
		t.Fatalf("an omitted field changed the list to %v", got)
	}

	// Bad entry: 400 naming it, and the stored list is untouched.
	rec = participantsPatch(t, h, `{"default_attendee_emails":["ok@example.com","not an email"]}`, key)
	mustStatus(t, rec, http.StatusBadRequest, "patch bad entry")
	if !strings.Contains(rec.Body.String(), "not an email") {
		t.Errorf("400 body does not name the bad entry: %s", rec.Body.String())
	}
	if got := participantsGet(t, h, key); len(got) != 2 {
		t.Fatalf("a rejected patch changed the list to %v", got)
	}

	// [] clears.
	mustStatus(t, participantsPatch(t, h, `{"default_attendee_emails":[]}`, key), http.StatusOK, "patch clear")
	if got := participantsGet(t, h, key); len(got) != 0 {
		t.Fatalf("[] left %v; want cleared", got)
	}
}

func TestParticipantSettings_requireAdmin(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	rawKey := "non-admin-participants-key"
	if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','other@example.com','Other','UTC',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','test',?,'2024-01-01')`, sha256HexForTest(rawKey)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetParticipantSettings)(rec, authReq(http.MethodGet, "/v1/settings/participants", "", rawKey))
	mustStatus(t, rec, http.StatusForbidden, "non-admin get")

	rec = participantsPatch(t, h, `{"default_attendee_emails":["bot@example.com"]}`, rawKey)
	mustStatus(t, rec, http.StatusForbidden, "non-admin patch")
}

// The point of the feature: every host calendar event created for a booking carries the
// default participants - minus anyone already on the event (the booker, a host).
func TestBookingHostEvent_carriesDefaultParticipants(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, key)
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, userID); err != nil {
		t.Fatal(err)
	}
	p := attendeeCalendar{created: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)

	// The host (host@example.com, from setupWorkspaceWithDB) and the booker are both listed
	// as defaults: neither may be invited a second time.
	mustStatus(t, participantsPatch(t, h,
		`{"default_attendee_emails":["notes@example.com","Host@example.com","booker@example.com"]}`, key),
		http.StatusOK, "patch participants")

	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"Booker","email":"Booker@example.com"}`, slug)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	mustStatus(t, rec, http.StatusCreated, "create booking")

	params := awaitCreate(t, p)
	if want := []string{"notes@example.com"}; !reflect.DeepEqual(params.ExtraAttendees, want) {
		t.Fatalf("ExtraAttendees = %v; want %v (booker and host excluded)", params.ExtraAttendees, want)
	}
	if params.OrganizerEmail != "Booker@example.com" {
		t.Errorf("OrganizerEmail = %q; the booker must stay the organizer attendee", params.OrganizerEmail)
	}
}

// With no defaults configured the params are exactly what they were before the feature.
func TestBookingHostEvent_noDefaultsMeansNoExtraAttendees(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, key)
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, userID); err != nil {
		t.Fatal(err)
	}
	p := attendeeCalendar{created: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)

	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"Booker","email":"booker@example.com"}`, slug)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	mustStatus(t, rec, http.StatusCreated, "create booking")

	if params := awaitCreate(t, p); params.ExtraAttendees != nil {
		t.Fatalf("ExtraAttendees = %v; want nil when nothing is configured", params.ExtraAttendees)
	}
}

// Reassign recreates the event on the new host's calendar; that event carries the defaults
// too, minus the new host and the booker.
func TestReassignBooking_newHostEventCarriesDefaultParticipants(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`,
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','h3@example.com','Host3','UTC',0)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','Intro',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		 VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh1','b1','u2',1)`,
		`INSERT INTO booking_attendees (id,booking_id,name,email,iana_timezone,is_organizer)
		 VALUES ('a1','b1','Alice','alice@example.com','UTC',1)`,
		`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination)
		 VALUES ('conn3','u3','google','test','primary',1)`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	p := attendeeCalendar{created: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(database)
	svc.Register(p)
	h.SetCalendar(svc)
	mustStatus(t, participantsPatch(t, h,
		`{"default_attendee_emails":["notes@example.com","h3@example.com","alice@example.com"]}`, ownerKey),
		http.StatusOK, "patch participants")

	req := authReq(http.MethodPost, "/v1/bookings/b1/reassign", `{"host_id":"u3"}`, ownerKey)
	req.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	mustStatus(t, rec, http.StatusOK, "reassign")

	params := awaitCreate(t, p)
	if want := []string{"notes@example.com"}; !reflect.DeepEqual(params.ExtraAttendees, want) {
		t.Fatalf("ExtraAttendees = %v; want %v (new host and booker excluded)", params.ExtraAttendees, want)
	}
}

// Guard against the JSON shape drifting: the API contract is a plain list under one key.
func TestParticipantSettings_shape(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	rec := participantsPatch(t, h, `{"default_attendee_emails":["bot@example.com"]}`, key)
	mustStatus(t, rec, http.StatusOK, "patch")
	var out struct {
		DefaultAttendeeEmails []string `json:"default_attendee_emails"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(out.DefaultAttendeeEmails, []string{"bot@example.com"}) {
		t.Fatalf("got %v", out.DefaultAttendeeEmails)
	}
}

// Both fields are validated before either is written: a bad message must not leave a
// new participant list behind (the form submits both together).
func TestParticipantSettings_validatesBothBeforeWriting(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)
	mustStatus(t, participantsPatch(t, h, `{"default_attendee_emails":["keep@example.com"],"default_calendar_message":"<p>keep</p>"}`, key), http.StatusOK, "seed")
	tooLong := strings.Repeat("x", 20001)
	rec := participantsPatch(t, h, `{"default_attendee_emails":["new@example.com"],"default_calendar_message":"`+tooLong+`"}`, key)
	mustStatus(t, rec, http.StatusBadRequest, "bad message with a valid list")
	if got := participantsGet(t, h, key); !reflect.DeepEqual(got, []string{"keep@example.com"}) {
		t.Errorf("a rejected patch wrote the participant list: %v", got)
	}
	rec = participantsPatch(t, h, `{"default_attendee_emails":["not an email"],"default_calendar_message":"<p>new</p>"}`, key)
	mustStatus(t, rec, http.StatusBadRequest, "bad list with a valid message")
	var msg string
	database.QueryRow(`SELECT default_calendar_message FROM server_settings WHERE id = 1`).Scan(&msg) //nolint:errcheck
	if msg != "<p>keep</p>" {
		t.Errorf("a rejected patch wrote the message: %q", msg)
	}
}
