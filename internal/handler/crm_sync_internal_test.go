package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
)

// meetHealingProvider heals the missing event the way Google does when asked for Meet:
// it returns the minted link and reports the event's iCalUID.
type meetHealingProvider struct{ healingProvider }

func (p *meetHealingProvider) CreateEvent(ctx context.Context, user string, in calendar.CreateEventParams) (string, string, string, error) {
	_, _, _, _ = p.healingProvider.CreateEvent(ctx, user, in)
	if in.ICalUID != nil {
		*in.ICalUID = "healed-1@google.com"
	}
	return "healed-1", "https://meet.google.com/abc-defg-hij", "primary", nil
}

// PRD P0.2: when the calendar sweep recreates a booking's event later and the calendar
// mints the Meet link then, a booking.updated webhook says so, with the new link, the
// iCalUID and changed: [meeting, location_value]. Before, the link was stored silently
// and a receiver held none.
func TestReconcile_healedMeetLinkFiresBookingUpdated(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	start := time.Now().UTC().Add(24 * time.Hour)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, name, iana_timezone) VALUES ('host-1', 'host@example.com', 'Host', 'UTC')`, nil},
		{`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, location_type) VALUES ('et-1', 'host-1', 'demo', 'Demo', 30, 'google_meet')`, nil},
		{`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, location_type) VALUES ('bk-1', 'et-1', 'host-1', ?, ?, 'confirmed', '2020-01-01T00:00:00Z', 'google_meet')`,
			[]any{start.Format(time.RFC3339), start.Add(30 * time.Minute).Format(time.RFC3339)}},
		{`INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, is_organizer, locale) VALUES ('a-1', 'bk-1', 'Alice', 'alice@example.com', 'UTC', 1, 'en')`, nil},
		{`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary) VALUES ('bh-1', 'bk-1', 'host-1', 1)`, nil},
		{`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn','host-1','google','test','primary',1)`, nil},
		{`INSERT INTO webhooks (id, user_id, url, events, secret_enc, scope) VALUES ('wh-1', 'host-1', 'https://hooks.example.com', '["booking.updated"]', 'x', 'org')`, nil},
	} {
		if _, err := database.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q.sql)
		}
	}
	var logs bytes.Buffer
	h := New(database, slog.New(slog.NewTextHandler(&logs, nil)))
	h.SetBaseURL("https://book.example.com")
	svc := calendar.NewService(database)
	svc.Register(&meetHealingProvider{})
	h.SetCalendar(svc)

	h.reconcileCreations(context.Background(), svc)

	var payload string
	if err := database.QueryRow(`SELECT payload FROM webhook_deliveries WHERE event = 'booking.updated'`).Scan(&payload); err != nil {
		t.Fatalf("no booking.updated delivery: %v (logs: %s)", err, logs.String())
	}
	var env struct {
		Data struct {
			Location string   `json:"location_value"`
			Changed  []string `json:"changed"`
			By       string   `json:"initiated_by"`
			AdminURL string   `json:"admin_url"`
			Meeting  struct {
				Provider        string `json:"provider"`
				JoinURL         string `json:"join_url"`
				ICalUID         string `json:"ical_uid"`
				CalendarEventID string `json:"calendar_event_id"`
			} `json:"meeting"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatal(err)
	}
	d := env.Data
	if d.Location != "https://meet.google.com/abc-defg-hij" || d.Meeting.JoinURL != d.Location || d.Meeting.Provider != "google_meet" {
		t.Errorf("location %q, meeting %+v; want the healed Meet link", d.Location, d.Meeting)
	}
	if d.Meeting.ICalUID != "healed-1@google.com" || d.Meeting.CalendarEventID != "healed-1" {
		t.Errorf("meeting %+v; want the healed event id and iCalUID", d.Meeting)
	}
	if strings.Join(d.Changed, ",") != "meeting,location_value" || d.By != "system" {
		t.Errorf("changed %v by %q; want [meeting location_value] by system", d.Changed, d.By)
	}
	if d.AdminURL != "https://book.example.com/admin/bookings/bk-1" {
		t.Errorf("admin_url = %q", d.AdminURL)
	}
}
