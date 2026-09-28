package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// The calendar message is admin HTML that lands in other people's calendars, so the API
// stores the sanitized form and a GET returns exactly what will be sent.
func TestPatchEventType_calendarMessage_sanitizedRoundTripAndClearable(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)

	code, out := patchET(t, h, apiKey, slug, `{"calendar_message":"<p>Bring <b>ID</b></p><script>alert(1)</script><a href=\"javascript:x\">no</a>"}`)
	if code != http.StatusOK {
		t.Fatalf("patch: %d — %v", code, out)
	}
	got, _ := out["calendar_message"].(string)
	if got != "<p>Bring <b>ID</b></p>no" {
		t.Errorf("calendar_message = %q; want the sanitized HTML", got)
	}

	// Whitespace / markup-only clears it; the JSON then reads null, like an unset row.
	code, out = patchET(t, h, apiKey, slug, `{"calendar_message":"<p> </p>"}`)
	if code != http.StatusOK || out["calendar_message"] != nil {
		t.Errorf("clearing: code=%d calendar_message=%v; want 200 and null", code, out["calendar_message"])
	}

	// A save that does not mention the field leaves it alone (whole-form editor rule).
	if code, _ := patchET(t, h, apiKey, slug, `{"calendar_message":"<p>keep</p>"}`); code != http.StatusOK {
		t.Fatal("seed keep")
	}
	code, out = patchET(t, h, apiKey, slug, `{"name":"Renamed"}`)
	if code != http.StatusOK || out["calendar_message"] != "<p>keep</p>" {
		t.Errorf("untouched field: code=%d calendar_message=%v; want 200 and <p>keep</p>", code, out["calendar_message"])
	}
}

func TestPatchEventType_calendarMessage_tooLongIsRejected(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	big := strings.Repeat("a", 10001)
	code, out := patchET(t, h, apiKey, slug, fmt.Sprintf(`{"calendar_message":%q}`, big))
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "calendar_message") {
		t.Errorf("got %d %v; want 400 naming calendar_message", code, out)
	}
}

func TestDuplicateEventType_copiesTheCalendarMessage(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	memberID := seedMember(t, database, "u2", "member@example.com")
	seedRichEventType(t, database, ownerID, memberID)
	if _, err := database.Exec(`UPDATE event_types SET calendar_message = '<p>Agenda</p>' WHERE slug = 'intro-call'`); err != nil {
		t.Fatal(err)
	}
	rec := duplicate(t, h, ownerKey, "intro-call")
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate: %d — %s", rec.Code, rec.Body.String())
	}
	var got struct {
		CalendarMessage *string `json:"calendar_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CalendarMessage == nil || *got.CalendarMessage != "<p>Agenda</p>" {
		t.Errorf("calendar_message on the copy = %v; want <p>Agenda</p>", got.CalendarMessage)
	}
}

// Booking creation puts the message on the host's calendar event: plain text with the
// Booking ID last for every provider, and the HTML twin for the ones that render it.
func TestCreateBooking_calendarEventCarriesTheMessage(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	slug, etID := seedEventTypeHTTP(t, h, key)
	if _, err := db.Exec(`UPDATE event_types SET calendar_message = '<p>Read the <a href="https://x.io/brief">brief</a></p>' WHERE id = ?`, etID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, userID); err != nil {
		t.Fatal(err)
	}
	p := telephoneCalendar{events: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)

	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"Test","email":"test@example.com"}`, slug)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	select {
	case ev := <-p.events:
		wantPlain := "Read the brief (https://x.io/brief)\n\nBooking ID: " + resp.ID
		if ev.Description != wantPlain {
			t.Errorf("Description = %q; want %q", ev.Description, wantPlain)
		}
		wantRich := `<p>Read the <a href="https://x.io/brief" rel="nofollow noopener" target="_blank">brief</a></p><p>Booking ID: ` + resp.ID + `</p>`
		if ev.DescriptionHTML != wantRich {
			t.Errorf("DescriptionHTML = %q; want %q", ev.DescriptionHTML, wantRich)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("calendar event was never created")
	}
}

// Reassignment recreates the event on the new host's calendar with the same composed
// description, so the message follows the booking to whoever hosts it.
func TestReassignBooking_newHostEventCarriesTheMessage(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`)                                   //nolint:errcheck
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','h3@example.com','Host3','UTC',0)`)                                   //nolint:errcheck
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes,calendar_message) VALUES ('et1','u2','et-slug','Intro',30,'<p>Agenda</p>')`) //nolint:errcheck
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`) //nolint:errcheck
	database.Exec(`INSERT INTO booking_attendees (id,booking_id,name,email,iana_timezone,is_organizer)
		VALUES ('a1','b1','Alice','alice@example.com','UTC',1)`) //nolint:errcheck
	database.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn3','u3','google','test','primary',1)`) //nolint:errcheck
	database.Exec(`INSERT INTO event_type_questions (id,event_type_id,label,type,position) VALUES ('q1','et1','Topic','text',0)`)                                         //nolint:errcheck
	database.Exec(`INSERT INTO booking_answers (id,booking_id,question_id,value) VALUES ('ans1','b1','q1','Pricing')`)                                                    //nolint:errcheck
	p := telephoneCalendar{events: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(database)
	svc.Register(p)
	h.SetCalendar(svc)

	req := authReq(http.MethodPost, "/v1/bookings/b1/reassign", `{"host_id":"u3"}`, ownerKey)
	req.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign: got %d — %s", rec.Code, rec.Body.String())
	}
	select {
	case ev := <-p.events:
		if ev.Description != "Agenda\n\nTopic: Pricing\n\nBooking ID: b1" || ev.DescriptionHTML != "<p>Agenda</p><p><strong>Topic:</strong> Pricing</p><p>Booking ID: b1</p>" {
			t.Errorf("new host event: plain=%q rich=%q", ev.Description, ev.DescriptionHTML)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event created for the new host")
	}
}

// Email notes are rich text now: saved sanitized, with a roomier cap for markup.
func TestPatchEventType_emailNotes_sanitizedAndCapped(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	for _, f := range []string{"msg_confirmation", "msg_cancellation", "msg_reschedule", "msg_reminder"} {
		code, out := patchET(t, h, apiKey, slug, fmt.Sprintf(`{%q:"<p>See <em>you</em></p><img src=x onerror=alert(1)>"}`, f))
		if code != http.StatusOK || out[f] != "<p>See <em>you</em></p>" {
			t.Errorf("%s: code=%d value=%v; want sanitized <p>See <em>you</em></p>", f, code, out[f])
		}
		code, out = patchET(t, h, apiKey, slug, fmt.Sprintf(`{%q:%q}`, f, strings.Repeat("x", 4001)))
		if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), f) {
			t.Errorf("%s over cap: code=%d out=%v; want 400 naming the field", f, code, out)
		}
		if code, _ := patchET(t, h, apiKey, slug, fmt.Sprintf(`{%q:%q}`, f, strings.Repeat("x", 4000))); code != http.StatusOK {
			t.Errorf("%s at cap: code=%d; want 200", f, code)
		}
	}
}

// The booker's answers ride on the host's calendar event, between the message and
// the Booking ID, so the host has the context without opening the admin app.
func TestCreateBooking_calendarEventCarriesQuestionAnswers(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	slug, etID := seedEventTypeHTTP(t, h, key)
	if _, err := db.Exec(`UPDATE event_types SET calendar_message = '<p>Agenda</p>' WHERE id = ?`, etID); err != nil {
		t.Fatal(err)
	}
	q1 := createQuestion(t, h, slug, key, `{"label":"Company","type":"text"}`)
	q2 := createQuestion(t, h, slug, key, `{"label":"Newsletter","type":"checkbox"}`)
	q3 := createQuestion(t, h, slug, key, `{"label":"Left blank","type":"text"}`)
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, userID); err != nil {
		t.Fatal(err)
	}
	p := telephoneCalendar{events: make(chan calendar.CreateEventParams, 1)}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)

	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"Test","email":"test@example.com",
		"answers":[{"question_id":%q,"value":"Acme & Co"},{"question_id":%q,"value":"yes"},{"question_id":%q,"value":"  "}]}`, slug, q1, q2, q3)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	select {
	case ev := <-p.events:
		wantPlain := "Agenda\n\nCompany: Acme & Co\nNewsletter: yes\n\nBooking ID: " + resp.ID
		if ev.Description != wantPlain {
			t.Errorf("Description = %q; want %q", ev.Description, wantPlain)
		}
		wantRich := "<p>Agenda</p><p><strong>Company:</strong> Acme &amp; Co</p><p><strong>Newsletter:</strong> yes</p><p>Booking ID: " + resp.ID + "</p>"
		if ev.DescriptionHTML != wantRich {
			t.Errorf("DescriptionHTML = %q; want %q", ev.DescriptionHTML, wantRich)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("calendar event was never created")
	}
}
