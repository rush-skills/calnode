package handler

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/slots"
)

// healingProvider records the params of every event the reconcile sweep creates.
type healingProvider struct {
	stuckProvider
	mu      sync.Mutex
	created []calendar.CreateEventParams
}

func (p *healingProvider) Name() string                                         { return "google" }
func (p *healingProvider) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *healingProvider) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *healingProvider) CreateEvent(_ context.Context, _ string, in calendar.CreateEventParams) (string, string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, in)
	return "healed-1", "", "primary", nil
}

// The reconcile sweep heals a missing host event with the SAME composed description the
// inline path writes — message first, Booking ID last — so a healed event is
// indistinguishable from one created at booking time.
func TestReconcileCreations_healedEventCarriesTheCalendarMessage(t *testing.T) {
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
		{`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, calendar_message) VALUES ('et-1', 'host-1', 'intro', 'Intro', 30, '<p>Agenda</p>')`, nil},
		{`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at) VALUES ('bk-1', 'et-1', 'host-1', ?, ?, 'confirmed', '2020-01-01T00:00:00Z')`,
			[]any{start.Format(time.RFC3339), start.Add(30 * time.Minute).Format(time.RFC3339)}},
		{`INSERT INTO booking_attendees (id, booking_id, name, email, iana_timezone, is_organizer, locale) VALUES ('a-1', 'bk-1', 'Alice', 'alice@example.com', 'UTC', 1, 'en')`, nil},
		{`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary) VALUES ('bh-1', 'bk-1', 'host-1', 1)`, nil},
		{`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES ('conn','host-1','google','test','primary',1)`, nil},
		{`INSERT INTO event_type_questions (id,event_type_id,label,type,position) VALUES ('q-1','et-1','Topic','text',0)`, nil},
		{`INSERT INTO booking_answers (id,booking_id,question_id,value) VALUES ('ans-1','bk-1','q-1','Pricing')`, nil},
	} {
		if _, err := database.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q.sql)
		}
	}
	var logs bytes.Buffer
	h := New(database, slog.New(slog.NewTextHandler(&logs, nil)))
	p := &healingProvider{}
	svc := calendar.NewService(database)
	svc.Register(p)
	h.SetCalendar(svc)

	h.reconcileCreations(context.Background(), svc)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.created) != 1 {
		t.Fatalf("created %d events; want 1 (logs: %s)", len(p.created), logs.String())
	}
	ev := p.created[0]
	if ev.Description != "Agenda\n\nTopic: Pricing\n\nBooking ID: bk-1" || ev.DescriptionHTML != "<p>Agenda</p><p><strong>Topic:</strong> Pricing</p><p>Booking ID: bk-1</p>" {
		t.Errorf("healed event: plain=%q rich=%q", ev.Description, ev.DescriptionHTML)
	}
	var stored string
	if err := database.QueryRow(`SELECT external_event_id FROM booking_hosts WHERE id = 'bh-1'`).Scan(&stored); err != nil || stored != "healed-1" {
		t.Errorf("external_event_id = %q (err %v); want healed-1", stored, err)
	}
}
