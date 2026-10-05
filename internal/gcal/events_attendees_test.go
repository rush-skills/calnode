package gcal

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// Default participants are sent as plain attendees after the organizer, on the same
// sendUpdates=all request, so Google emails them the invite like it does the booker.
func TestCreateEvent_extraAttendeesAreInvited(t *testing.T) {
	srv, gotReq := mockCreateEventServer(t, "ev1", http.StatusOK)

	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")

	p := calendar.CreateEventParams{
		Summary:        "Team Sync with Alice",
		Start:          time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC),
		End:            time.Date(2026, 6, 20, 15, 0, 0, 0, time.UTC),
		OrganizerName:  "Alice",
		OrganizerEmail: "alice@example.com",
		ExtraAttendees: []string{"notes@example.com", "bot@example.com"},
	}
	if _, _, _, err := c.CreateEvent(context.Background(), "user-1", p); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	want := []calEventAttendee{
		{Email: "alice@example.com", DisplayName: "Alice"},
		{Email: "notes@example.com"},
		{Email: "bot@example.com"},
	}
	if len(gotReq.Attendees) != len(want) {
		t.Fatalf("Attendees = %+v; want %+v", gotReq.Attendees, want)
	}
	for i := range want {
		if gotReq.Attendees[i] != want[i] {
			t.Errorf("attendee %d = %+v; want %+v", i, gotReq.Attendees[i], want[i])
		}
	}
}

// Without an organizer (no booker email) the defaults still go on the event.
func TestCreateEvent_extraAttendeesWithoutOrganizer(t *testing.T) {
	srv, gotReq := mockCreateEventServer(t, "ev1", http.StatusOK)

	c := newTestClient(t)
	c.apiBase = srv.URL
	saveDestinationConnection(t, c, "user-1", "primary")

	p := calendar.CreateEventParams{
		Summary:        "Sync",
		Start:          time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC),
		End:            time.Date(2026, 6, 20, 15, 0, 0, 0, time.UTC),
		ExtraAttendees: []string{"notes@example.com"},
	}
	if _, _, _, err := c.CreateEvent(context.Background(), "user-1", p); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if len(gotReq.Attendees) != 1 || gotReq.Attendees[0].Email != "notes@example.com" {
		t.Fatalf("Attendees = %+v; want just notes@example.com", gotReq.Attendees)
	}
}
