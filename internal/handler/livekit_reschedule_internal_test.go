package handler

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/livekit"
)

func newLiveKitTestHandler(t *testing.T, withLiveKit bool) *Handler {
	t.Helper()
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	h := New(database, slog.Default())
	h.baseURL = "https://calnode.example.com"
	if withLiveKit {
		h.SetLiveKit(livekit.New("wss://lk.example.com", "key", "secret", [32]byte{1, 2, 3}))
	}
	ctx := context.Background()
	database.ExecContext(ctx,
		`INSERT INTO users (id, email, name, iana_timezone) VALUES ('u1','h@example.com','Host','UTC')`) //nolint:errcheck
	database.ExecContext(ctx,
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('et1','u1','meet','Meet',30)`) //nolint:errcheck
	return h
}

func seedLiveKitBooking(t *testing.T, h *Handler, end time.Time, room, location string) booking.Booking {
	t.Helper()
	ctx := context.Background()
	start := end.Add(-30 * time.Minute)
	_, err := h.db.ExecContext(ctx,
		`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, location_value, livekit_room)
		 VALUES ('b1','et1','u1',?,?, 'confirmed',?,?)`,
		start.Format(time.RFC3339), end.Format(time.RFC3339), location, room)
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return booking.Booking{ID: "b1", EventTypeID: "et1", HostID: "u1",
		StartAt: start, EndAt: end, Status: "confirmed", LocationValue: location}
}

// TestRemintLiveKitLinks_refreshesExpiredURLs is the #98 regression: after a reschedule
// past the original end, the stored join URL must carry the new expiry.
func TestRemintLiveKitLinks_refreshesExpiredURLs(t *testing.T) {
	h := newLiveKitTestHandler(t, true)
	ctx := context.Background()
	lk := h.getLiveKit()

	oldEnd := time.Now().UTC().Add(-time.Hour) // original meeting already over
	oldURL := lk.BookingJoinURL(h.baseURL, "booking-b1", "", oldEnd.Add(2*time.Hour))
	b := seedLiveKitBooking(t, h, oldEnd, "booking-b1", oldURL)

	// Reschedule to tomorrow: same room, new end.
	b.EndAt = time.Now().UTC().Add(25 * time.Hour).Truncate(time.Second)
	b.StartAt = b.EndAt.Add(-30 * time.Minute)
	h.remintLiveKitLinks(ctx, &b)

	var stored string
	if err := h.db.QueryRowContext(ctx, `SELECT location_value FROM bookings WHERE id = 'b1'`).Scan(&stored); err != nil {
		t.Fatalf("read location: %v", err)
	}
	if stored == oldURL {
		t.Fatal("location_value unchanged after reschedule; want a re-minted URL")
	}
	if b.LocationValue != stored {
		t.Error("in-memory booking not updated alongside the stored row")
	}
	u, err := url.Parse(stored)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if !strings.HasPrefix(u.Path, "/room/booking-b1") {
		t.Errorf("URL path = %q; want the same room", u.Path)
	}
	lc := livekit.New("wss://lk.example.com", "key", "secret", [32]byte{1, 2, 3})
	_, _, exp, err := lc.VerifyRoomToken(u.Query().Get("t"))
	if err != nil {
		t.Fatalf("fresh token does not verify: %v", err)
	}
	if !exp.Equal(b.EndAt.Add(2 * time.Hour)) {
		t.Errorf("token expiry = %v; want new end + 2h", exp)
	}
}

func TestRemintLiveKitLinks_skipsWhenNothingToDo(t *testing.T) {
	ctx := context.Background()
	end := time.Now().UTC().Add(time.Hour)

	// LiveKit disabled: untouched.
	h := newLiveKitTestHandler(t, false)
	b := seedLiveKitBooking(t, h, end, "booking-b1", "https://calnode.example.com/room/booking-b1?t=old")
	h.remintLiveKitLinks(ctx, &b)
	var stored string
	h.db.QueryRowContext(ctx, `SELECT location_value FROM bookings WHERE id = 'b1'`).Scan(&stored) //nolint:errcheck
	if stored != "https://calnode.example.com/room/booking-b1?t=old" {
		t.Errorf("location changed with LiveKit disabled: %q", stored)
	}

	// LiveKit on but no room minted (non-LiveKit booking): untouched.
	h2 := newLiveKitTestHandler(t, true)
	b2 := seedLiveKitBooking(t, h2, end, "", "https://meet.example.com/manual")
	h2.remintLiveKitLinks(ctx, &b2)
	h2.db.QueryRowContext(ctx, `SELECT location_value FROM bookings WHERE id = 'b1'`).Scan(&stored) //nolint:errcheck
	if stored != "https://meet.example.com/manual" {
		t.Errorf("non-LiveKit location changed: %q", stored)
	}
}

// locProvider records the location each UpdateEvent carried.
type locProvider struct {
	calendar.Provider
	mu        sync.Mutex
	locations []string
}

func (p *locProvider) Name() string                                         { return "google" }
func (p *locProvider) InvitesGuests() bool                                  { return true }
func (p *locProvider) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *locProvider) UpdateEvent(_ context.Context, _, _, _ string, _, _ time.Time, location string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.locations = append(p.locations, location)
	return nil
}

// After a LiveKit reschedule every host's calendar event must carry the re-minted
// (attendee-safe) link as its location; a booking with no room moves with "" (unchanged).
func TestMoveCalendarEvents_carriesRemintedLiveKitLink(t *testing.T) {
	h := newLiveKitTestHandler(t, true)
	ctx := context.Background()
	p := &locProvider{}
	svc := calendar.NewService(h.db)
	svc.Register(p)
	h.SetCalendar(svc)
	for _, q := range []string{
		`INSERT INTO users (id, email, name, iana_timezone) VALUES ('u2','c@example.com','Cohost','UTC')`,
		`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('c1','u1','google','t','primary',1)`,
		`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('c2','u2','google','t','primary',1)`,
	} {
		if _, err := h.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	oldEnd := time.Now().UTC().Add(-time.Hour)
	b := seedLiveKitBooking(t, h, oldEnd, "booking-b1", "https://calnode.example.com/room/booking-b1?t=old")
	for _, q := range []string{
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh1','b1','u1',1,'evt-1','google')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh2','b1','u2',0,'evt-2','google')`,
	} {
		if _, err := h.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	b.EndAt = time.Now().UTC().Add(25 * time.Hour).Truncate(time.Second)
	b.StartAt = b.EndAt.Add(-30 * time.Minute)
	newLocation := ""
	if h.remintLiveKitLinks(ctx, &b) {
		newLocation = b.LocationValue
	}
	if newLocation == "" || strings.Contains(newLocation, "t=old") || strings.Contains(newLocation, "host") {
		t.Fatalf("re-minted attendee link = %q", newLocation)
	}
	h.moveCalendarEvents(ctx, b.ID, b.StartAt, b.EndAt, newLocation)
	if len(p.locations) != 2 || p.locations[0] != newLocation || p.locations[1] != newLocation {
		t.Errorf("host events moved with locations %v; want both = %q", p.locations, newLocation)
	}

	// No room: the location argument stays empty, so providers leave it alone.
	h2 := newLiveKitTestHandler(t, false)
	if h2.remintLiveKitLinks(ctx, &b) {
		t.Error("remint without LiveKit reported true")
	}
}
