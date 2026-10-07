package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
)

// linkCalendar is telephoneCalendar plus the update/cancel calls a reschedule makes.
type linkCalendar struct{ telephoneCalendar }

func (linkCalendar) UpdateEvent(context.Context, string, string, string, time.Time, time.Time, string) error {
	return nil
}
func (linkCalendar) CancelEvent(context.Context, string, string, string) error { return nil }

var manageLinkRE = regexp.MustCompile(`https://book\.example\.com/manage/([0-9a-f]{64})\?action=reschedule`)

// With no Calnode email, the calendar invite is the booker's only message. It must carry
// working reschedule and cancel links, and those links must keep working after the
// booker reschedules, because the event description is not rewritten then.
func TestCalendarInvite_manageLinksSurviveReschedule(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	h.SetBaseURL("https://book.example.com")
	slug, _ := seedEventTypeHTTP(t, h, key)
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn',?,'google','test','primary',1)`, userID); err != nil {
		t.Fatal(err)
	}
	p := linkCalendar{telephoneCalendar{events: make(chan calendar.CreateEventParams, 4)}}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)
	if _, err := db.Exec(`INSERT INTO webhooks (id, user_id, url, events, secret_enc) VALUES ('wh1', ?, 'https://hooks.example.com/x', '["booking.rescheduled"]', 'x')`, userID); err != nil {
		t.Fatal(err)
	}

	bookingID := createBookingViaHTTP(t, h, slug, "2026-06-20T09:00:00Z")
	var ev calendar.CreateEventParams
	select {
	case ev = <-p.events:
	case <-time.After(5 * time.Second):
		t.Fatal("calendar event was never created")
	}

	m := manageLinkRE.FindStringSubmatch(ev.Description)
	if m == nil {
		t.Fatalf("plain description has no reschedule link:\n%s", ev.Description)
	}
	tok := m[1]
	link := "https://book.example.com/manage/" + tok
	for _, want := range []string{"Need to make a change?", "Reschedule: " + link + "?action=reschedule", "Cancel: " + link + "?action=cancel"} {
		if !strings.Contains(ev.Description, want) {
			t.Errorf("plain description missing %q:\n%s", want, ev.Description)
		}
	}
	if !strings.HasSuffix(ev.Description, "Booking ID: "+bookingID) {
		t.Errorf("Booking ID must stay last:\n%s", ev.Description)
	}
	if !strings.Contains(ev.DescriptionHTML, `<a href="`+link+`?action=cancel">Cancel booking</a>`) {
		t.Errorf("rich description missing the cancel link:\n%s", ev.DescriptionHTML)
	}

	// An email link issued now is rotated away by the reschedule; the calendar one is not.
	emailTok := issueTestToken(t, db, bookingID)

	req := httptest.NewRequest(http.MethodPost, "/manage/"+tok+"/reschedule", strings.NewReader(`{"start_at":"2026-06-20T10:00:00Z"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("token", tok)
	rec := httptest.NewRecorder()
	h.RescheduleByToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reschedule through the calendar link: %d %s", rec.Code, rec.Body)
	}

	// The webhook is the last side effect; once it is queued, the rotation has run.
	var payload string
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.QueryRow(`SELECT payload FROM webhook_deliveries WHERE event = 'booking.rescheduled'`).Scan(&payload)
		if err == nil {
			break
		}
		if err != sql.ErrNoRows || time.Now().After(deadline) {
			t.Fatalf("booking.rescheduled was never queued: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(payload, `"initiated_by":"booker"`) {
		t.Errorf("webhook payload %s; want initiated_by booker", payload)
	}

	bs := booking.New(db)
	if _, err := bs.ValidateManageToken(context.Background(), tok); err != nil {
		t.Errorf("calendar link stopped working after the reschedule: %v", err)
	}
	if _, err := bs.ValidateManageToken(context.Background(), emailTok); err == nil {
		t.Error("the pre-reschedule email link still works; it should have been rotated")
	}

	// The manage page renders for the calendar link (the ?action deep link is client-side).
	req = httptest.NewRequest(http.MethodGet, "/manage/"+tok+"?action=cancel", nil)
	req.SetPathValue("token", tok)
	rec = httptest.NewRecorder()
	h.ManagePage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="cancel-btn"`) {
		t.Errorf("manage page via calendar link: %d", rec.Code)
	}
}

// A manage link must outlive the meeting it manages, however far ahead it was booked.
func TestManageToken_expiryCoversTheMeeting(t *testing.T) {
	h, db, key, _ := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, key)
	bookingID := createBookingViaHTTP(t, h, slug, "2026-06-20T09:00:00Z")
	end := time.Now().UTC().Add(200 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.Exec(`UPDATE bookings SET start_at = ?, end_at = ? WHERE id = ?`,
		end.Add(-30*time.Minute).Format(time.RFC3339), end.Format(time.RFC3339), bookingID); err != nil {
		t.Fatal(err)
	}
	bs := booking.New(db)
	for _, issue := range []func(context.Context, string) (string, error){bs.IssueManageToken, bs.IssueCalendarManageToken} {
		if _, err := issue(context.Background(), bookingID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`SELECT purpose, expires_at FROM booking_manage_tokens WHERE booking_id = ?`, bookingID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var purpose, exp string
		if err := rows.Scan(&purpose, &exp); err != nil {
			t.Fatal(err)
		}
		got, _ := time.Parse(time.RFC3339, exp)
		if got.Before(end) {
			t.Errorf("%s token expires %s, before the meeting ends at %s", purpose, exp, end.Format(time.RFC3339))
		}
		n++
	}
	if n < 2 {
		t.Fatalf("found %d tokens; want both purposes", n)
	}
}
