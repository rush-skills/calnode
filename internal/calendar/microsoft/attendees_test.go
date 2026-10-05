package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// Default participants are sent to Graph as required attendees after the organizer, so they
// get the native invite and show on the Teams roster.
func TestCreateEvent_extraAttendeesAreRequired(t *testing.T) {
	c := newTestClient(t)
	connect(t, c, "u1")

	var got graphEventReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"evt-1"}`))
	}))
	defer srv.Close()
	c.apiBase = srv.URL

	start := time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC)
	if _, _, _, err := c.CreateEvent(context.Background(), "u1", calendar.CreateEventParams{
		Summary:        "Intro call",
		Start:          start,
		End:            start.Add(30 * time.Minute),
		OrganizerName:  "Alex",
		OrganizerEmail: "alex@example.com",
		ExtraAttendees: []string{"notes@example.com", "bot@example.com"},
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	if len(got.Attendees) != 3 {
		t.Fatalf("attendees = %+v; want organizer + 2 defaults", got.Attendees)
	}
	if got.Attendees[0].EmailAddress.Address != "alex@example.com" || got.Attendees[0].EmailAddress.Name != "Alex" {
		t.Errorf("organizer attendee = %+v", got.Attendees[0])
	}
	for i, want := range []string{"notes@example.com", "bot@example.com"} {
		a := got.Attendees[i+1]
		if a.EmailAddress.Address != want || a.Type != "required" || a.EmailAddress.Name != "" {
			t.Errorf("attendee %d = %+v; want %s, type required, no name", i+1, a, want)
		}
	}
}
