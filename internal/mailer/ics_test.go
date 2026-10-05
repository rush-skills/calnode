package mailer

import (
	"context"
	"strings"
	"testing"
)

func TestBuildICS_request(t *testing.T) {
	d := testBookingData()
	d.ICSSequence = 0
	got := string(BuildICS(d, "REQUEST"))

	for _, want := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "METHOD:REQUEST",
		"BEGIN:VEVENT", "UID:01J4TEST@calnode", "SEQUENCE:0",
		"DTSTART:20260615T090000Z", "DTEND:20260615T093000Z",
		"SUMMARY:30-Minute Call", "STATUS:CONFIRMED",
		"ORGANIZER;CN=\"Alice Host\":mailto:host@example.com",
		"ATTENDEE;CN=\"Bob Booker\";RSVP=TRUE:mailto:bob@example.com",
		"END:VEVENT", "END:VCALENDAR",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ICS missing %q\n---\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "END:VCALENDAR\r\n") {
		t.Error("ICS must use CRLF line endings")
	}
}

func TestBuildICS_cancel(t *testing.T) {
	d := testBookingData()
	got := string(BuildICS(d, "CANCEL"))
	if !strings.Contains(got, "METHOD:CANCEL") {
		t.Error("cancel ICS missing METHOD:CANCEL")
	}
	if !strings.Contains(got, "STATUS:CANCELLED") {
		t.Error("cancel ICS missing STATUS:CANCELLED")
	}
}

func TestBuildICS_escapesText(t *testing.T) {
	d := testBookingData()
	d.EventTypeName = "Strategy, Planning; Q3"
	got := string(BuildICS(d, "REQUEST"))
	if !strings.Contains(got, `SUMMARY:Strategy\, Planning\; Q3`) {
		t.Errorf("commas/semicolons not escaped in SUMMARY:\n%s", got)
	}
}

func TestBuildICS_foldsLongLines(t *testing.T) {
	d := testBookingData()
	d.LocationValue = "https://meet.example.com/" + strings.Repeat("x", 200)
	got := string(BuildICS(d, "REQUEST"))
	for _, line := range strings.Split(got, "\r\n") {
		// Continuation lines start with a space; every physical line must be ≤75 octets.
		if len(line) > 75 {
			t.Errorf("unfolded line exceeds 75 octets (%d): %q", len(line), line)
		}
	}
	// And it must still rejoin to contain the full URL when unfolded.
	unfolded := strings.ReplaceAll(got, "\r\n ", "")
	if !strings.Contains(unfolded, d.LocationValue) {
		t.Error("folded LOCATION does not unfold back to the original URL")
	}
}

func TestSendConfirmationToAttendee_attachesICSWhenFlagged(t *testing.T) {
	cap := &captureMailer{}
	d := testBookingData()
	d.AttachICS = true
	if err := SendConfirmationToAttendee(context.Background(), cap, d); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg := cap.all()[0]
	if len(msg.Attachments) != 1 {
		t.Fatalf("got %d attachments; want 1", len(msg.Attachments))
	}
	a := msg.Attachments[0]
	if a.Filename != "invite.ics" {
		t.Errorf("attachment filename = %q; want invite.ics", a.Filename)
	}
	if !strings.Contains(a.ContentType, "text/calendar") || !strings.Contains(a.ContentType, "method=REQUEST") {
		t.Errorf("attachment content-type = %q; want text/calendar method=REQUEST", a.ContentType)
	}
}

func TestSendConfirmationToAttendee_noICSByDefault(t *testing.T) {
	cap := &captureMailer{}
	if err := SendConfirmationToAttendee(context.Background(), cap, testBookingData()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n := len(cap.all()[0].Attachments); n != 0 {
		t.Errorf("got %d attachments; want 0 when AttachICS is false", n)
	}
}

func TestSendConfirmationToHost_attachesICSWhenFlagged(t *testing.T) {
	cap := &captureMailer{}
	d := testBookingData()
	d.AttachICS = true
	if err := SendConfirmationToHost(context.Background(), cap, d); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg := cap.all()[0]
	if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "invite.ics" {
		t.Fatalf("host email should carry the invite.ics attachment; got %d", len(msg.Attachments))
	}
}

func TestBuildRaw_multipartWithICS(t *testing.T) {
	s := smtpForTest()
	d := testBookingData()
	d.AttachICS = true
	raw := string(mustBuildRaw(t, s, Message{
		To:          []string{"bob@example.com"},
		Subject:     "Booking confirmed",
		Text:        "body text",
		Attachments: []Attachment{icsAttachment(d, "REQUEST")},
	}))

	if !strings.Contains(raw, "Content-Type: multipart/mixed; boundary=") {
		t.Error("missing multipart/mixed content type")
	}
	if !strings.Contains(raw, "Content-Type: text/calendar; charset=utf-8; method=REQUEST") {
		t.Error("missing text/calendar part")
	}
	if !strings.Contains(raw, `Content-Disposition: attachment; filename="invite.ics"`) {
		t.Error("missing attachment disposition")
	}
	if !strings.Contains(raw, "body text") {
		t.Error("missing text body part")
	}
}

// The invite message rides at the top of the .ics DESCRIPTION and the add-to-calendar
// links, so the attendee's own calendar entry reads like the host's Google/Outlook one.
func TestBuildICSAndCalendarLinks_carryTheCalendarMessage(t *testing.T) {
	d := testBookingData()
	d.CalendarMessage = "Agenda\n- One"
	d.ManageURL = "https://calnode.example.com/manage/tok"

	ics := string(BuildICS(d, "REQUEST"))
	// ICS folds long lines and escapes newlines as \n; the order is what matters.
	if !strings.Contains(ics, "DESCRIPTION:Agenda\\n- One\\n\\n") {
		t.Errorf("ICS DESCRIPTION should open with the message:\n%s", ics)
	}
	if strings.Index(ics, "Agenda") > strings.Index(ics, "Alice") {
		t.Errorf("message should precede the host line:\n%s", ics)
	}
	for _, u := range []string{d.GoogleCalURL(), d.OutlookCalURL()} {
		if !strings.Contains(u, "Agenda%0A-+One%0A%0A") {
			t.Errorf("calendar link should carry the message first: %s", u)
		}
	}

	// Without a message, nothing changes.
	d.CalendarMessage = ""
	if strings.Contains(string(BuildICS(d, "REQUEST")), "Agenda") || strings.Contains(d.GoogleCalURL(), "Agenda") {
		t.Error("message leaked with CalendarMessage empty")
	}
}
