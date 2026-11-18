package handler

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/slots"
)

func mustRule(t *testing.T, s *liveSeries) liveRule {
	t.Helper()
	r, err := s.rule()
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	return r
}

func strp(s string) *string { return &s }

// 17:00 Europe/Sofia stays 17:00 on the wall clock across the end of summer time
// (25 Oct 2026): 14:00Z before, 15:00Z after.
func TestLiveRule_wallClockAcrossDST(t *testing.T) {
	s := &liveSeries{Timezone: "Europe/Sofia", Weekdays: []int{2, 4}, IntervalWeeks: 1, StartTime: "17:00",
		DurationMinutes: 60, StartDate: "2026-10-20"}
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	got := mustRule(t, s).occurrences(from, from.Add(10*24*time.Hour))
	want := map[string]int{"2026-10-20": 14, "2026-10-22": 14, "2026-10-27": 15, "2026-10-29": 15}
	if len(got) != len(want) {
		t.Fatalf("occurrences = %+v; want %d", got, len(want))
	}
	for _, o := range got {
		if h, ok := want[o.Date]; !ok || o.Start.Hour() != h || o.End.Sub(o.Start) != time.Hour {
			t.Errorf("%s starts %s; want %02d:00Z for an hour", o.Date, o.Start.Format(time.RFC3339), h)
		}
	}
}

// Skipped weeks produce nothing, and the rotation index counts held sessions only, so a
// skipped week does not skip a host.
func TestLiveRule_skipsIntervalAndRotationIndex(t *testing.T) {
	s := &liveSeries{Timezone: "Europe/Sofia", Weekdays: []int{1}, IntervalWeeks: 1, StartTime: "10:00",
		DurationMinutes: 30, StartDate: "2026-12-07", SkipDates: []string{"2026-12-21/2026-12-27", "2026-12-28"},
		HostUserIDs: []string{"drushya", "prashant"}}
	from := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	got := mustRule(t, s).occurrences(from, time.Date(2027, 1, 20, 0, 0, 0, 0, time.UTC))
	var dates, hosts []string
	for _, o := range got {
		dates = append(dates, o.Date)
		hosts = append(hosts, s.hostFor(o.Index))
	}
	if want := []string{"2026-12-07", "2026-12-14", "2027-01-04", "2027-01-11", "2027-01-18"}; !slices.Equal(dates, want) {
		t.Errorf("dates = %v; want %v", dates, want)
	}
	if want := []string{"drushya", "prashant", "drushya", "prashant", "drushya"}; !slices.Equal(hosts, want) {
		t.Errorf("hosts = %v; want %v", hosts, want)
	}

	// Every other week from the first date's week, with an end date.
	s2 := &liveSeries{Timezone: "UTC", Weekdays: []int{3}, IntervalWeeks: 2, StartTime: "09:00", DurationMinutes: 60,
		StartDate: "2026-11-04", EndDate: strp("2026-12-02")}
	got = mustRule(t, s2).occurrences(from.AddDate(0, -1, 0), from.AddDate(1, 0, 0))
	dates = nil
	for _, o := range got {
		dates = append(dates, o.Date)
	}
	if want := []string{"2026-11-04", "2026-11-18", "2026-12-02"}; !slices.Equal(dates, want) {
		t.Errorf("fortnightly dates = %v; want %v", dates, want)
	}
}

// seriesCalendar records creates, cancels and time updates.
type seriesCalendar struct {
	calendar.Provider
	mu      sync.Mutex
	creates []calendar.CreateEventParams
	cancels int
	n       int
}

func (p *seriesCalendar) Name() string                                         { return "google" }
func (p *seriesCalendar) InvitesGuests() bool                                  { return true }
func (p *seriesCalendar) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *seriesCalendar) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *seriesCalendar) CreateEvent(_ context.Context, _ string, in calendar.CreateEventParams) (string, string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, in)
	p.n++
	link := ""
	if in.AddMeet {
		link = "https://meet.google.com/fresh-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+p.n%26))
	}
	return "evt-" + in.Summary, link, "primary", nil
}
func (p *seriesCalendar) UpdateEvent(context.Context, string, string, string, time.Time, time.Time, string) error {
	return nil
}
func (p *seriesCalendar) CancelEvent(context.Context, string, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels++
	return nil
}

func seriesFixture(t *testing.T) (*Handler, *sql.DB, *seriesCalendar) {
	t.Helper()
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"drushya", "prashant", "robert"} {
		if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES (?,?,?,'UTC',1)`, u, u+"@example.com", u); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES (?,?,'google','t','primary',1)`, "c-"+u, u); err != nil {
			t.Fatal(err)
		}
	}
	h := New(database, slog.Default())
	p := &seriesCalendar{}
	svc := calendar.NewService(database)
	svc.Register(p)
	h.SetCalendar(svc)
	h.liveEventAttendeeSource = func(context.Context) ([]string, error) { return []string{"team@example.com"}, nil }
	return h, database, p
}

func seriesOccurrences(t *testing.T, h *Handler, seriesID string) []*liveEvent {
	t.Helper()
	rows, err := h.db.Query(liveEventSelect+` WHERE e.series_id = ? ORDER BY e.occurrence_date`, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*liveEvent
	for rows.Next() {
		ev, err := scanLiveEvent(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func newTestSeries(t *testing.T, h *Handler, now time.Time, mut func(*liveSeries)) *liveSeries {
	t.Helper()
	ts := liveTime(now)
	s := &liveSeries{ID: "s1", Title: "Onboarding", Kind: "weekly_onboarding", HostUserIDs: []string{"drushya", "prashant"},
		Timezone: "Europe/Sofia", Weekdays: []int{2, 4}, IntervalWeeks: 1, StartTime: "17:00", DurationMinutes: 60,
		StartDate: "2026-11-01", SkipDates: []string{}, AutoStart: true, AutoEnd: true, InviteDefaultParticipants: true,
		Status: liveSeriesActive, CreatedBy: "robert", CreatedAt: ts, UpdatedAt: ts}
	if mut != nil {
		mut(s)
	}
	if err := h.saveLiveSeries(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	return s
}

// Occurrences within the horizon become ordinary live events, each with its own id,
// schedule, rotating host and freshly minted Meet; a second pass adds nothing.
func TestLiveSeries_materialise(t *testing.T) {
	h, _, cal := seriesFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) // a Monday
	s := newTestSeries(t, h, now, func(s *liveSeries) { s.InviteDefaultParticipants = false })

	if n := h.materializeLiveSeries(ctx, s, now); n != 8 {
		t.Fatalf("materialised %d occurrences; want 8 (Tue+Thu over four weeks)", n)
	}
	if n := h.materializeLiveSeries(ctx, s, now); n != 0 {
		t.Errorf("second pass materialised %d; want 0", n)
	}
	occ := seriesOccurrences(t, h, s.ID)
	links := map[string]bool{}
	for i, ev := range occ {
		if want := s.HostUserIDs[i%2]; ev.HostUserID != want {
			t.Errorf("occurrence %s host = %s; want %s", *ev.OccurrenceDate, ev.HostUserID, want)
		}
		if !ev.JoinURLMinted || ev.JoinURL == "" || links[ev.JoinURL] {
			t.Errorf("occurrence %s link = %q minted=%v; want a fresh minted link", *ev.OccurrenceDate, ev.JoinURL, ev.JoinURLMinted)
		}
		links[ev.JoinURL] = true
		if ev.Kind != "weekly_onboarding" || ev.InviteDefaultParticipants {
			t.Errorf("occurrence fields = %+v", ev)
		}
	}
	for _, c := range cal.creates {
		if len(c.ExtraAttendees) != 0 {
			t.Errorf("default participants invited with the switch off: %v", c.ExtraAttendees)
		}
	}
	// 17:00 Sofia, winter time: 15:00Z.
	if got := *occ[0].ScheduledStartAt; got != "2026-11-03T15:00:00Z" {
		t.Errorf("first start = %s; want 2026-11-03T15:00:00Z", got)
	}
}

// A standing link is reused by every occurrence and nothing is minted.
func TestLiveSeries_fixedLink(t *testing.T) {
	h, _, cal := seriesFixture(t)
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := newTestSeries(t, h, now, func(s *liveSeries) { s.JoinURL = "https://meet.google.com/open-hours"; s.Weekdays = []int{3} })
	h.materializeLiveSeries(context.Background(), s, now)
	for _, ev := range seriesOccurrences(t, h, s.ID) {
		if ev.JoinURL != s.JoinURL || ev.JoinURLMinted {
			t.Errorf("occurrence link = %q minted=%v; want the series link", ev.JoinURL, ev.JoinURLMinted)
		}
	}
	for _, c := range cal.creates {
		if c.AddMeet || c.Location != s.JoinURL {
			t.Errorf("calendar event minted a link or lost the standing one: %+v", c)
		}
	}
}

// Editing the series updates future occurrences field by field: an occurrence whose host
// was changed on its own keeps that host but takes the new title; a cancelled date stays
// cancelled; a newly skipped date disappears.
func TestLiveSeries_resyncRespectsOverridesAndCancellations(t *testing.T) {
	h, database, _ := seriesFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := newTestSeries(t, h, now, nil)
	h.materializeLiveSeries(ctx, s, now)
	occ := seriesOccurrences(t, h, s.ID)

	// Occurrence 0: host changed on its own. Occurrence 1: cancelled.
	if _, err := database.Exec(`UPDATE live_events SET host_user_id = 'robert', overrides = 'host' WHERE id = ?`, occ[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := h.cancelLiveEventNow(ctx, occ[1], now); err != nil {
		t.Fatal(err)
	}

	s.Title = "Weekly onboarding"
	s.HostUserIDs = []string{"prashant"}
	s.SkipDates = []string{*occ[2].OccurrenceDate}
	h.resyncLiveSeries(ctx, s, now)

	after := map[string]*liveEvent{}
	for _, ev := range seriesOccurrences(t, h, s.ID) {
		after[*ev.OccurrenceDate] = ev
	}
	if ev := after[*occ[0].OccurrenceDate]; ev.HostUserID != "robert" || ev.Title != "Weekly onboarding" {
		t.Errorf("overridden occurrence = host %s title %q; want robert / new title", ev.HostUserID, ev.Title)
	}
	if ev := after[*occ[1].OccurrenceDate]; ev.Status != liveStatusCancelled {
		t.Errorf("cancelled occurrence came back as %s", ev.Status)
	}
	if _, ok := after[*occ[2].OccurrenceDate]; ok {
		t.Errorf("skipped date %s still has an occurrence", *occ[2].OccurrenceDate)
	}
	if ev := after[*occ[3].OccurrenceDate]; ev.HostUserID != "prashant" || ev.Title != "Weekly onboarding" || !ev.HasCalendarEvent {
		t.Errorf("plain occurrence = %+v; want the new host and title, with a calendar event", ev)
	}
	// Ending the series cancels what is still scheduled.
	h.cancelFutureSeriesOccurrences(ctx, s.ID, now)
	for _, ev := range seriesOccurrences(t, h, s.ID) {
		if ev.Status != liveStatusCancelled {
			t.Errorf("after ending, occurrence %s is %s", *ev.OccurrenceDate, ev.Status)
		}
	}
}

// The sweep materialises and starts an occurrence that has just come due.
func TestLiveSeries_sweepMaterialisesAndStarts(t *testing.T) {
	h, _, _ := seriesFixture(t)
	created := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := newTestSeries(t, h, created, func(s *liveSeries) { s.Weekdays = []int{1}; s.StartTime = "11:00"; s.Timezone = "UTC" })
	h.SweepLiveEvents(context.Background(), created.Add(2*time.Hour+time.Minute)) // Mon 11:01Z
	occ := seriesOccurrences(t, h, s.ID)
	if len(occ) == 0 || occ[0].Status != liveStatusLive {
		t.Fatalf("first occurrence = %+v; want live", occ)
	}
}
