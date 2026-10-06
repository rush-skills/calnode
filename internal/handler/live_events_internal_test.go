package handler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/slots"
)

// attendeeCalendar records the ExtraAttendees of each CreateEvent.
type attendeeCalendar struct {
	calendar.Provider
	got [][]string
}

func (p *attendeeCalendar) Name() string                                         { return "google" }
func (p *attendeeCalendar) InvitesGuests() bool                                  { return true }
func (p *attendeeCalendar) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *attendeeCalendar) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *attendeeCalendar) CreateEvent(_ context.Context, _ string, in calendar.CreateEventParams) (string, string, string, error) {
	p.got = append(p.got, in.ExtraAttendees)
	return "evt", "https://meet.google.com/x", "primary", nil
}

// The workspace's default participants (the notetaker bot) are invited on the session's
// calendar event, minus the host's own address, and never duplicated.
func TestLiveEventCalendar_invitesDefaultParticipantsMinusHost(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('host','Host@Example.com','Host','UTC',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('c','host','google','t','primary',1)`); err != nil {
		t.Fatal(err)
	}
	h := New(database, slog.Default())
	p := &attendeeCalendar{}
	svc := calendar.NewService(database)
	svc.Register(p)
	h.SetCalendar(svc)
	h.liveEventAttendeeSource = func(context.Context) ([]string, error) {
		return []string{"team@example.com", "host@example.com", " Team@Example.com ", "notes@example.com"}, nil
	}

	now := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	if _, err := database.Exec(`INSERT INTO live_events (id,title,host_user_id,created_by,created_at,updated_at) VALUES ('le','Office hours','host','host',?,?)`,
		liveTime(now), liveTime(now)); err != nil {
		t.Fatal(err)
	}
	ev, err := h.loadLiveEvent(context.Background(), "le")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ensureLiveEventCalendar(context.Background(), ev, now); err != nil {
		t.Fatalf("ensureLiveEventCalendar: %v", err)
	}
	if len(p.got) != 1 {
		t.Fatalf("creates = %d; want 1", len(p.got))
	}
	want := []string{"team@example.com", "notes@example.com"}
	if got := p.got[0]; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ExtraAttendees = %v; want %v (host dropped, duplicates folded)", got, want)
	}
	if ev.JoinURL != "https://meet.google.com/x" || ev.externalEventID != "evt" || ev.externalProvider != "google" {
		t.Errorf("event not adopted: %+v", ev)
	}
	// Window with no scheduled end: start + default duration.
	start, end := liveEventWindow(ev, now)
	if !start.Equal(now) || !end.Equal(now.Add(liveEventDefaultDuration)) {
		t.Errorf("window = %v–%v", start, end)
	}
}

// Without the override the production source is used; on a workspace with none it is
// simply an empty guest list, never an error that blocks the session.
func TestLiveEventExtraAttendees_noDefaultsIsEmpty(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	h := New(database, slog.Default())
	if got := h.liveEventExtraAttendees(context.Background(), "host@example.com"); len(got) != 0 {
		t.Errorf("extra attendees = %v; want none", got)
	}
}
