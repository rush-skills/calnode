package caldav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The assertions read the object through the package's own unfold (ics.go), the way a CalDAV
// client does: an ATTENDEE line is longer than 75 octets and so is folded on the wire.

// Default participants become ATTENDEE lines with RSVP=TRUE, after the ORGANIZER, inside the
// VEVENT that is PUT to the server.
func TestCreateEvent_extraAttendeesWrittenAsAttendeeLines(t *testing.T) {
	var mu sync.Mutex
	var putBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			putBody = string(b)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t)
	ctx := context.Background()
	seedUser(t, c.db, "u1")
	if err := c.saveConnection(ctx, "u1", "a@a.test", "pw-a", srv.URL+"/calendars/a/home/", ""); err != nil {
		t.Fatal(err)
	}
	svc := newSvc(c)
	if err := svc.SetDestination(ctx, "u1", "caldav", "a@a.test"); err != nil {
		t.Fatal(err)
	}

	p := bookingParams()
	p.ExtraAttendees = []string{"notes@example.com", "bot@example.com"}
	eventID, _, _, _, err := svc.CreateEvent(ctx, "u1", p)
	if err != nil || eventID == "" {
		t.Fatalf("CreateEvent: id=%q err=%v", eventID, err)
	}

	mu.Lock()
	body := putBody
	mu.Unlock()
	lines := unfold(body)
	var org, attendees []string
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "ORGANIZER"):
			org = append(org, ln)
		case strings.HasPrefix(ln, "ATTENDEE"):
			attendees = append(attendees, ln)
		}
	}
	if len(org) != 1 || !strings.HasSuffix(org[0], ":mailto:booker@x.test") {
		t.Errorf("ORGANIZER lines = %v; the booker must stay the organizer", org)
	}
	want := []string{
		"ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:notes@example.com",
		"ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:bot@example.com",
	}
	if strings.Join(attendees, "\n") != strings.Join(want, "\n") {
		t.Errorf("ATTENDEE lines:\n%s\nwant:\n%s\n\nfull body:\n%s", strings.Join(attendees, "\n"), strings.Join(want, "\n"), body)
	}
}

// Without defaults the object has no ATTENDEE line at all - byte-for-byte the pre-feature shape.
func TestBuildICS_noAttendeesWithoutDefaults(t *testing.T) {
	ics := buildICS("id", time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		"Intro", "", "", "Booker", "booker@x.test", nil, 0)
	if strings.Contains(ics, "ATTENDEE") {
		t.Errorf("unexpected ATTENDEE line:\n%s", ics)
	}
	// RFC 4791 §4.1: a stored calendar object resource must not carry METHOD (that is for
	// iTIP messages, i.e. the emailed .ics); with ATTENDEE lines present strict servers
	// would otherwise read the object as an invitation and refuse it.
	if strings.Contains(ics, "METHOD") {
		t.Errorf("stored object must not carry METHOD:\n%s", ics)
	}
}

// A reschedule rewrites only the time lines of the stored object, so the attendees survive it.
func TestRewriteEventTimes_keepsAttendeeLines(t *testing.T) {
	ics := buildICS("id", time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		"Intro", "", "", "Booker", "booker@x.test", []string{"notes@example.com"}, 0)
	moved := strings.Join(unfold(rewriteEventTimes(ics, moveStart, moveEnd, "")), "\n")
	if !strings.Contains(moved, "RSVP=TRUE:mailto:notes@example.com") {
		t.Errorf("attendee dropped on reschedule:\n%s", moved)
	}
	if !strings.Contains(moved, "DTSTART:"+icsUTC(moveStart)) {
		t.Errorf("start not moved:\n%s", moved)
	}
}
