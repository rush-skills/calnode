package handler

// Recurring live events (fork: docs/features/live-events.md, "Recurring series").
//
// A series is a weekly rule in a named timezone: weekdays, every N weeks, a wall-clock
// start time and a duration, a first date and an optional last one, and dates to skip.
// It never becomes "live" itself. The sweep materialises each occurrence a few weeks
// ahead (liveSeriesHorizon) as an ordinary live_events row with its own id, so the public
// feed, the widget, the calendar minting and start/end/cancel all work unchanged on it.
//
// An occurrence is tied to its series by (series_id, occurrence_date), unique, so a date
// is materialised once and a cancelled date stays cancelled. Editing one occurrence
// records which field groups it changed (overrides); editing the series then updates the
// remaining fields of every future scheduled occurrence and leaves the overridden ones
// alone. Times are wall clock in the series' timezone, so 17:00 Europe/Sofia stays
// 17:00 across daylight saving.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/uid"
)

const (
	// liveSeriesHorizon is how far ahead occurrences exist as rows. Far enough for a site
	// to list the coming weeks; near enough that a series edit touches a handful of rows
	// and calendar events.
	liveSeriesHorizon = 28 * 24 * time.Hour

	liveSeriesActive = "active"
	liveSeriesEnded  = "ended"

	liveSeriesMaxHosts     = 20
	liveSeriesMaxSkips     = 200
	liveSeriesMaxDuration  = 24 * 60
	liveSeriesMaxInterval  = 52
	liveSeriesMaxSpanYears = 5
)

// Override groups: what an occurrence-level edit can take over from its series.
const (
	liveOverrideTitle       = "title"
	liveOverrideDescription = "description"
	liveOverrideKind        = "kind"
	liveOverrideSchedule    = "schedule"
	liveOverrideHost        = "host"
	liveOverrideLink        = "link"
	liveOverrideAuto        = "auto"
	liveOverrideInvite      = "invite"
)

func splitOverrides(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// addOverrides returns cur plus every group changed[g] marks, sorted and unique.
func addOverrides(cur []string, changed map[string]bool) []string {
	set := map[string]bool{}
	for _, g := range cur {
		set[g] = true
	}
	for g, ok := range changed {
		if ok {
			set[g] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func hasOverride(ev *liveEvent, g string) bool { return slices.Contains(ev.Overrides, g) }

// ---------------------------------------------------------------------------
// The rule
// ---------------------------------------------------------------------------

var (
	reHHMM = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)
	reYMD  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func parseYMD(s string) (time.Time, bool) {
	if !reYMD.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// liveSkip is one skipped date or inclusive range of dates, as civil dates (UTC midnight).
type liveSkip struct{ from, to time.Time }

func parseSkips(in []string) ([]liveSkip, error) {
	out := make([]liveSkip, 0, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		a, b, isRange := strings.Cut(raw, "/")
		from, ok := parseYMD(strings.TrimSpace(a))
		if !ok {
			return nil, fmt.Errorf("skip date %q must be YYYY-MM-DD or YYYY-MM-DD/YYYY-MM-DD", raw)
		}
		to := from
		if isRange {
			if to, ok = parseYMD(strings.TrimSpace(b)); !ok || to.Before(from) {
				return nil, fmt.Errorf("skip range %q must be YYYY-MM-DD/YYYY-MM-DD with the end on or after the start", raw)
			}
		}
		out = append(out, liveSkip{from, to})
	}
	return out, nil
}

// liveRule is a series' recurrence, parsed and validated.
type liveRule struct {
	loc        *time.Location
	weekdays   map[time.Weekday]bool
	interval   int
	hour, min  int
	duration   time.Duration
	start, end time.Time // civil dates (UTC midnight); end zero = open
	skips      []liveSkip
}

// liveOccurrence is one date the rule produces. Index counts the occurrences the rule has
// produced since its first date (skipped dates excluded), which is what host rotation
// keys on, so a skipped week does not skip a host.
type liveOccurrence struct {
	Date       string
	Index      int
	Start, End time.Time
}

func (r liveRule) skipped(d time.Time) bool {
	for _, s := range r.skips {
		if !d.Before(s.from) && !d.After(s.to) {
			return true
		}
	}
	return false
}

// occurrences returns the occurrences whose window overlaps (from, to]: ending after from
// and starting no later than to.
func (r liveRule) occurrences(from, to time.Time) []liveOccurrence {
	var out []liveOccurrence
	// Weeks are counted from the Monday on or before the first date, so "every 2 weeks"
	// means the first date's week and every other week after it.
	weekday := (int(r.start.Weekday()) + 6) % 7 // Monday = 0
	anchor := r.start.AddDate(0, 0, -weekday)
	lastDay := to.In(r.loc)
	limit := time.Date(lastDay.Year(), lastDay.Month(), lastDay.Day(), 0, 0, 0, 0, time.UTC)
	if !r.end.IsZero() && r.end.Before(limit) {
		limit = r.end
	}
	index := 0
	for d := r.start; !d.After(limit); d = d.AddDate(0, 0, 1) {
		if !r.weekdays[d.Weekday()] {
			continue
		}
		if week := int(d.Sub(anchor).Hours()/24) / 7; week%r.interval != 0 {
			continue
		}
		if r.skipped(d) {
			continue
		}
		i := index
		index++
		start := time.Date(d.Year(), d.Month(), d.Day(), r.hour, r.min, 0, 0, r.loc)
		end := start.Add(r.duration)
		if !end.After(from) || start.After(to) {
			continue
		}
		out = append(out, liveOccurrence{Date: d.Format("2006-01-02"), Index: i, Start: start.UTC(), End: end.UTC()})
	}
	return out
}

// ---------------------------------------------------------------------------
// The series row
// ---------------------------------------------------------------------------

type liveSeriesHost struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type liveSeries struct {
	ID                        string           `json:"id"`
	Title                     string           `json:"title"`
	Description               string           `json:"description"`
	Kind                      string           `json:"kind"`
	HostUserIDs               []string         `json:"host_user_ids"`
	Hosts                     []liveSeriesHost `json:"hosts"`
	Timezone                  string           `json:"timezone"`
	Weekdays                  []int            `json:"weekdays"`
	IntervalWeeks             int              `json:"interval_weeks"`
	StartTime                 string           `json:"start_time"`
	DurationMinutes           int              `json:"duration_minutes"`
	StartDate                 string           `json:"start_date"`
	EndDate                   *string          `json:"end_date"`
	SkipDates                 []string         `json:"skip_dates"`
	JoinURL                   string           `json:"join_url"`
	AutoStart                 bool             `json:"auto_start"`
	AutoEnd                   bool             `json:"auto_end"`
	InviteDefaultParticipants bool             `json:"invite_default_participants"`
	Status                    string           `json:"status"`
	CreatedBy                 string           `json:"created_by"`
	CreatedAt                 string           `json:"created_at"`
	UpdatedAt                 string           `json:"updated_at"`
}

const liveSeriesSelect = `
	SELECT id, title, description, kind, host_user_ids, timezone, weekdays, interval_weeks, start_time,
	       duration_minutes, start_date, end_date, skip_dates, join_url, auto_start, auto_end,
	       invite_default_participants, status, created_by, created_at, updated_at
	FROM live_event_series`

func scanLiveSeries(row liveEventScanner) (*liveSeries, error) {
	var s liveSeries
	var hosts, weekdays, skips string
	var autoStart, autoEnd, invite int
	if err := row.Scan(&s.ID, &s.Title, &s.Description, &s.Kind, &hosts, &s.Timezone, &weekdays, &s.IntervalWeeks,
		&s.StartTime, &s.DurationMinutes, &s.StartDate, &s.EndDate, &skips, &s.JoinURL, &autoStart, &autoEnd,
		&invite, &s.Status, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(hosts), &s.HostUserIDs)
	_ = json.Unmarshal([]byte(weekdays), &s.Weekdays)
	_ = json.Unmarshal([]byte(skips), &s.SkipDates)
	if s.HostUserIDs == nil {
		s.HostUserIDs = []string{}
	}
	if s.SkipDates == nil {
		s.SkipDates = []string{}
	}
	s.AutoStart, s.AutoEnd, s.InviteDefaultParticipants = autoStart != 0, autoEnd != 0, invite != 0
	return &s, nil
}

func (h *Handler) loadLiveSeries(ctx context.Context, id string) (*liveSeries, error) {
	s, err := scanLiveSeries(h.db.QueryRowContext(ctx, liveSeriesSelect+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	h.fillLiveSeriesHosts(ctx, s)
	return s, nil
}

func (h *Handler) fillLiveSeriesHosts(ctx context.Context, s *liveSeries) {
	s.Hosts = []liveSeriesHost{}
	for _, id := range s.HostUserIDs {
		var name string
		_ = h.db.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM users WHERE id = ?`, id).Scan(&name)
		s.Hosts = append(s.Hosts, liveSeriesHost{id, name})
	}
}

// rule parses the stored series into its recurrence. A row written through the API always
// parses; an error here means a hand-edited row, and the series produces nothing.
func (s *liveSeries) rule() (liveRule, error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return liveRule{}, fmt.Errorf("timezone: %w", err)
	}
	m := reHHMM.FindStringSubmatch(s.StartTime)
	if m == nil {
		return liveRule{}, errors.New("start_time must be HH:MM")
	}
	r := liveRule{loc: loc, weekdays: map[time.Weekday]bool{}, interval: s.IntervalWeeks,
		duration: time.Duration(s.DurationMinutes) * time.Minute}
	r.hour, _ = strconv.Atoi(m[1]) // regexp-checked digits
	r.min, _ = strconv.Atoi(m[2])
	for _, d := range s.Weekdays {
		r.weekdays[time.Weekday(d)] = true
	}
	var ok bool
	if r.start, ok = parseYMD(s.StartDate); !ok {
		return liveRule{}, errors.New("start_date must be YYYY-MM-DD")
	}
	if s.EndDate != nil && *s.EndDate != "" {
		if r.end, ok = parseYMD(*s.EndDate); !ok {
			return liveRule{}, errors.New("end_date must be YYYY-MM-DD")
		}
	}
	if r.skips, err = parseSkips(s.SkipDates); err != nil {
		return liveRule{}, err
	}
	if r.interval < 1 {
		r.interval = 1
	}
	return r, nil
}

// hostFor is the host of the index-th occurrence: the one host, or the rotation.
func (s *liveSeries) hostFor(index int) string {
	if len(s.HostUserIDs) == 0 {
		return ""
	}
	return s.HostUserIDs[index%len(s.HostUserIDs)]
}

func canManageLiveSeries(u AuthUser, s *liveSeries) bool {
	return u.IsAdmin || s.CreatedBy == u.ID || slices.Contains(s.HostUserIDs, u.ID)
}

// ---------------------------------------------------------------------------
// Materialise and resync
// ---------------------------------------------------------------------------

// materializeLiveSeries inserts the occurrences within the horizon that have no row yet
// (any status: a cancelled date stays cancelled) and mints each new one's calendar event.
// Safe to run concurrently with itself: the unique (series_id, occurrence_date) index
// makes the insert a no-op for a date another pass already took.
func (h *Handler) materializeLiveSeries(ctx context.Context, s *liveSeries, now time.Time) int {
	if s.Status != liveSeriesActive {
		return 0
	}
	rule, err := s.rule()
	if err != nil {
		h.logger.Warn("live series: invalid rule, nothing materialised", "error", err, "series_id", s.ID)
		return 0
	}
	have := map[string]bool{}
	rows, err := h.db.QueryContext(ctx, `SELECT occurrence_date FROM live_events WHERE series_id = ?`, s.ID)
	if err != nil {
		h.logger.Error("live series: existing occurrences", "error", err, "series_id", s.ID)
		return 0
	}
	for rows.Next() {
		var d sql.NullString
		if rows.Scan(&d) == nil && d.Valid {
			have[d.String] = true
		}
	}
	rows.Close() // #nosec G104 -- drained; the inserts below need the single connection

	created := 0
	for _, occ := range rule.occurrences(now, now.Add(liveSeriesHorizon)) {
		if have[occ.Date] {
			continue
		}
		ts := liveTime(now)
		ev := &liveEvent{
			ID: uid.New(), Title: s.Title, Description: s.Description, Kind: s.Kind, Status: liveStatusScheduled,
			HostUserID: s.hostFor(occ.Index), ScheduledStartAt: liveTimePtr(occ.Start), ScheduledEndAt: liveTimePtr(occ.End),
			AutoStart: s.AutoStart, AutoEnd: s.AutoEnd, JoinURL: s.JoinURL,
			InviteDefaultParticipants: s.InviteDefaultParticipants,
			SeriesID:                  &s.ID, OccurrenceDate: &occ.Date,
			CreatedBy: s.CreatedBy, CreatedAt: ts, UpdatedAt: ts,
		}
		res, err := h.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO live_events (id, title, description, kind, status, host_user_id, scheduled_start_at,
			    scheduled_end_at, auto_start, auto_end, join_url, invite_default_participants, series_id, occurrence_date,
			    created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'scheduled', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.ID, ev.Title, ev.Description, ev.Kind, nullIfEmpty(ev.HostUserID), ev.ScheduledStartAt, ev.ScheduledEndAt,
			boolInt(ev.AutoStart), boolInt(ev.AutoEnd), ev.JoinURL, boolInt(ev.InviteDefaultParticipants), s.ID, occ.Date,
			ev.CreatedBy, ev.CreatedAt, ev.UpdatedAt)
		if err != nil {
			h.logger.Error("live series: insert occurrence", "error", err, "series_id", s.ID, "date", occ.Date)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // another pass took this date
		}
		created++
		if err := h.ensureLiveEventCalendar(ctx, ev, now); err != nil && !errors.Is(err, errLiveNoCalendar) {
			h.logger.Error("live series: calendar event for occurrence", "error", err, "live_event_id", ev.ID)
		}
	}
	return created
}

// resyncLiveSeries applies the series to its future scheduled occurrences after an edit:
// dates the rule no longer produces are removed (calendar event cancelled, row deleted),
// the others take every field they have not overridden, and missing dates are added.
// Occurrences that are live, ended or cancelled are history and are never touched.
func (h *Handler) resyncLiveSeries(ctx context.Context, s *liveSeries, now time.Time) {
	rule, err := s.rule()
	if err != nil {
		h.logger.Warn("live series: invalid rule, not resynced", "error", err, "series_id", s.ID)
		return
	}
	want := map[string]liveOccurrence{}
	if s.Status == liveSeriesActive {
		for _, occ := range rule.occurrences(now, now.Add(liveSeriesHorizon)) {
			want[occ.Date] = occ
		}
	}
	rows, err := h.db.QueryContext(ctx, liveEventSelect+`
		WHERE e.series_id = ? AND e.status = 'scheduled' AND e.scheduled_start_at > ?`, s.ID, liveTime(now))
	if err != nil {
		h.logger.Error("live series: load occurrences", "error", err, "series_id", s.ID)
		return
	}
	var evs []*liveEvent
	for rows.Next() {
		if ev, err := scanLiveEvent(rows); err == nil {
			evs = append(evs, ev)
		}
	}
	rows.Close() // #nosec G104 -- drained; the updates below need the single connection

	for _, ev := range evs {
		occ, ok := want[ptrString(ev.OccurrenceDate)]
		if !ok {
			// No longer part of the series (a skipped date, a shortened end, a removed
			// weekday). It was never live and nobody has its link yet: remove it.
			h.cancelLiveEventCalendar(ctx, ev)
			if _, err := h.db.ExecContext(ctx, `DELETE FROM live_events WHERE id = ? AND status = 'scheduled'`, ev.ID); err != nil {
				h.logger.Error("live series: remove occurrence", "error", err, "live_event_id", ev.ID)
			}
			continue
		}
		h.applySeriesToOccurrence(ctx, s, ev, occ, now)
	}
	h.materializeLiveSeries(ctx, s, now)
}

// applySeriesToOccurrence copies the series' fields onto one future occurrence, except
// the groups that occurrence overrides, and brings its calendar event along: a new host
// re-hosts it, a changed title, description, link or guest list re-creates it, a moved
// time moves it.
func (h *Handler) applySeriesToOccurrence(ctx context.Context, s *liveSeries, ev *liveEvent, occ liveOccurrence, now time.Time) {
	rebuild, moved := false, false
	if !hasOverride(ev, liveOverrideTitle) && ev.Title != s.Title {
		ev.Title, rebuild = s.Title, true
	}
	if !hasOverride(ev, liveOverrideDescription) && ev.Description != s.Description {
		ev.Description, rebuild = s.Description, true
	}
	if !hasOverride(ev, liveOverrideKind) {
		ev.Kind = s.Kind
	}
	if !hasOverride(ev, liveOverrideAuto) {
		ev.AutoStart, ev.AutoEnd = s.AutoStart, s.AutoEnd
	}
	if !hasOverride(ev, liveOverrideInvite) && ev.InviteDefaultParticipants != s.InviteDefaultParticipants {
		ev.InviteDefaultParticipants, rebuild = s.InviteDefaultParticipants, true
	}
	if !hasOverride(ev, liveOverrideSchedule) {
		start, end := liveTime(occ.Start), liveTime(occ.End)
		if ptrString(ev.ScheduledStartAt) != start || ptrString(ev.ScheduledEndAt) != end {
			ev.ScheduledStartAt, ev.ScheduledEndAt, moved = &start, &end, true
		}
	}
	if !hasOverride(ev, liveOverrideLink) {
		switch {
		case s.JoinURL != "" && (ev.JoinURL != s.JoinURL || ev.JoinURLMinted):
			ev.JoinURL, ev.JoinURLMinted, rebuild = s.JoinURL, false, true
		case s.JoinURL == "" && ev.JoinURL != "" && !ev.JoinURLMinted:
			// The series went from a standing link to a fresh one per session.
			ev.JoinURL, rebuild = "", true
		}
	}
	if rebuild && ev.JoinURLMinted {
		// A minted link belongs to the event being replaced; the new event mints its own.
		ev.JoinURL, ev.JoinURLMinted = "", false
	}
	newHost := ev.HostUserID
	if !hasOverride(ev, liveOverrideHost) {
		newHost = s.hostFor(occ.Index)
	}
	ev.UpdatedAt = liveTime(now)
	if _, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET title = ?, description = ?, kind = ?, scheduled_start_at = ?, scheduled_end_at = ?,
		       auto_start = ?, auto_end = ?, join_url = ?, join_url_minted = ?, invite_default_participants = ?,
		       updated_at = ? WHERE id = ? AND status = 'scheduled'`,
		ev.Title, ev.Description, ev.Kind, ev.ScheduledStartAt, ev.ScheduledEndAt,
		boolInt(ev.AutoStart), boolInt(ev.AutoEnd), ev.JoinURL, boolInt(ev.JoinURLMinted), boolInt(ev.InviteDefaultParticipants),
		ev.UpdatedAt, ev.ID); err != nil {
		h.logger.Error("live series: update occurrence", "error", err, "live_event_id", ev.ID)
		return
	}
	switch {
	case newHost != ev.HostUserID:
		if err := h.rehostLiveEvent(ctx, ev, newHost, now); err != nil {
			h.logger.Error("live series: rehost occurrence", "error", err, "live_event_id", ev.ID)
		}
	case rebuild:
		h.cancelLiveEventCalendar(ctx, ev)
		if err := h.ensureLiveEventCalendar(ctx, ev, now); err != nil && !errors.Is(err, errLiveNoCalendar) {
			h.logger.Error("live series: calendar event after series edit", "error", err, "live_event_id", ev.ID)
		}
	case moved:
		start, end := liveEventWindow(ev, now)
		h.syncLiveEventCalendarTime(ctx, ev, start, end)
	}
}

// materializeAllLiveSeries runs from the live-event sweep: every active series gets the
// occurrences that have come within the horizon since the last pass.
func (h *Handler) materializeAllLiveSeries(ctx context.Context, now time.Time) {
	rows, err := h.db.QueryContext(ctx, liveSeriesSelect+` WHERE status = 'active'`)
	if err != nil {
		h.logger.Error("live series: list active", "error", err)
		return
	}
	var all []*liveSeries
	for rows.Next() {
		if s, err := scanLiveSeries(rows); err == nil {
			all = append(all, s)
		}
	}
	rows.Close() // #nosec G104 -- drained before writing
	for _, s := range all {
		mu := h.liveEventLock("series:" + s.ID)
		mu.Lock()
		h.materializeLiveSeries(ctx, s, now)
		mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

type liveSeriesInput struct {
	Title                     *string   `json:"title"`
	Description               *string   `json:"description"`
	Kind                      *string   `json:"kind"`
	HostUserIDs               *[]string `json:"host_user_ids"`
	HostUserID                *string   `json:"host_user_id"` // shorthand for one host
	Timezone                  *string   `json:"timezone"`
	Weekdays                  *[]int    `json:"weekdays"`
	IntervalWeeks             *int      `json:"interval_weeks"`
	StartTime                 *string   `json:"start_time"`
	DurationMinutes           *int      `json:"duration_minutes"`
	StartDate                 *string   `json:"start_date"`
	EndDate                   *string   `json:"end_date"` // "" clears
	SkipDates                 *[]string `json:"skip_dates"`
	JoinURL                   *string   `json:"join_url"` // "" = a fresh link per occurrence
	AutoStart                 *bool     `json:"auto_start"`
	AutoEnd                   *bool     `json:"auto_end"`
	InviteDefaultParticipants *bool     `json:"invite_default_participants"`
}

// applyLiveSeriesInput validates the fields the request carries onto s (create: s holds
// the defaults). Writes the 400/403 and returns false on a bad field.
func (h *Handler) applyLiveSeriesInput(w http.ResponseWriter, r *http.Request, caller AuthUser, s *liveSeries, in liveSeriesInput) bool {
	bad := func(msg string) bool { h.writeError(w, http.StatusBadRequest, msg); return false }
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || len(t) > liveEventTitleMax {
			return bad("title is required and must be at most 200 characters")
		}
		s.Title = t
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if len(d) > liveEventDescMax {
			return bad("description is too long")
		}
		s.Description = d
	}
	if in.Kind != nil {
		k := strings.TrimSpace(*in.Kind)
		if !validLiveKind(k) {
			return bad("kind must be a short lowercase name: letters, digits, - or _ (e.g. office_hours, webinar)")
		}
		s.Kind = k
	}
	hosts := in.HostUserIDs
	if hosts == nil && in.HostUserID != nil {
		one := []string{*in.HostUserID}
		hosts = &one
	}
	if hosts != nil {
		var ids []string
		for _, id := range *hosts {
			if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			ids = []string{caller.ID}
		}
		if len(ids) > liveSeriesMaxHosts {
			return bad("too many hosts")
		}
		if !slices.Equal(ids, s.HostUserIDs) {
			for _, id := range ids {
				if id == caller.ID {
					continue
				}
				if !caller.IsAdmin {
					h.writeError(w, http.StatusForbidden, "only admins can assign another host")
					return false
				}
				var n int
				if err := h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE id = ? AND archived_at IS NULL`, id).Scan(&n); err != nil || n == 0 {
					return bad("host_user_ids must all be active members")
				}
			}
		}
		s.HostUserIDs = ids
	}
	if in.Timezone != nil {
		tz := strings.TrimSpace(*in.Timezone)
		if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
			return bad("timezone must be an IANA name such as Europe/Sofia")
		}
		s.Timezone = tz
	}
	if in.Weekdays != nil {
		set := map[int]bool{}
		for _, d := range *in.Weekdays {
			if d < 0 || d > 6 {
				return bad("weekdays are 0 (Sunday) to 6 (Saturday)")
			}
			set[d] = true
		}
		if len(set) == 0 {
			return bad("pick at least one weekday")
		}
		s.Weekdays = s.Weekdays[:0]
		for d := 0; d <= 6; d++ {
			if set[d] {
				s.Weekdays = append(s.Weekdays, d)
			}
		}
	}
	if in.IntervalWeeks != nil {
		if *in.IntervalWeeks < 1 || *in.IntervalWeeks > liveSeriesMaxInterval {
			return bad("interval_weeks must be between 1 and 52")
		}
		s.IntervalWeeks = *in.IntervalWeeks
	}
	if in.StartTime != nil {
		t := strings.TrimSpace(*in.StartTime)
		if !reHHMM.MatchString(t) {
			return bad("start_time must be HH:MM (24-hour)")
		}
		s.StartTime = t
	}
	if in.DurationMinutes != nil {
		if *in.DurationMinutes < 1 || *in.DurationMinutes > liveSeriesMaxDuration {
			return bad("duration_minutes must be between 1 and 1440")
		}
		s.DurationMinutes = *in.DurationMinutes
	}
	if in.StartDate != nil {
		d := strings.TrimSpace(*in.StartDate)
		if _, ok := parseYMD(d); !ok {
			return bad("start_date must be YYYY-MM-DD")
		}
		s.StartDate = d
	}
	if in.EndDate != nil {
		d := strings.TrimSpace(*in.EndDate)
		if d == "" {
			s.EndDate = nil
		} else {
			if _, ok := parseYMD(d); !ok {
				return bad("end_date must be YYYY-MM-DD")
			}
			s.EndDate = &d
		}
	}
	if s.EndDate != nil {
		start, _ := parseYMD(s.StartDate)
		end, _ := parseYMD(*s.EndDate)
		if end.Before(start) {
			return bad("end_date must be on or after start_date")
		}
		if end.After(start.AddDate(liveSeriesMaxSpanYears, 0, 0)) {
			return bad("a series can span at most 5 years")
		}
	}
	if in.SkipDates != nil {
		if len(*in.SkipDates) > liveSeriesMaxSkips {
			return bad("too many skip dates")
		}
		var clean []string
		for _, d := range *in.SkipDates {
			if d = strings.TrimSpace(d); d != "" {
				clean = append(clean, d)
			}
		}
		if _, err := parseSkips(clean); err != nil {
			return bad(err.Error())
		}
		sort.Strings(clean)
		if clean == nil {
			clean = []string{}
		}
		s.SkipDates = clean
	}
	if in.JoinURL != nil {
		j := strings.TrimSpace(*in.JoinURL)
		if j != "" && !validJoinURL(j) {
			return bad("join_url must be an http(s) URL")
		}
		s.JoinURL = j
	}
	if in.AutoStart != nil {
		s.AutoStart = *in.AutoStart
	}
	if in.AutoEnd != nil {
		s.AutoEnd = *in.AutoEnd
	}
	if in.InviteDefaultParticipants != nil {
		s.InviteDefaultParticipants = *in.InviteDefaultParticipants
	}
	return true
}

func (h *Handler) saveLiveSeries(ctx context.Context, s *liveSeries, insert bool) error {
	hosts, _ := json.Marshal(s.HostUserIDs)
	weekdays, _ := json.Marshal(s.Weekdays)
	skips, _ := json.Marshal(s.SkipDates)
	if insert {
		_, err := h.db.ExecContext(ctx, `
			INSERT INTO live_event_series (id, title, description, kind, host_user_ids, timezone, weekdays, interval_weeks,
			    start_time, duration_minutes, start_date, end_date, skip_dates, join_url, auto_start, auto_end,
			    invite_default_participants, status, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.Title, s.Description, s.Kind, string(hosts), s.Timezone, string(weekdays), s.IntervalWeeks,
			s.StartTime, s.DurationMinutes, s.StartDate, s.EndDate, string(skips), s.JoinURL, boolInt(s.AutoStart),
			boolInt(s.AutoEnd), boolInt(s.InviteDefaultParticipants), s.Status, s.CreatedBy, s.CreatedAt, s.UpdatedAt)
		return err
	}
	_, err := h.db.ExecContext(ctx, `
		UPDATE live_event_series SET title = ?, description = ?, kind = ?, host_user_ids = ?, timezone = ?, weekdays = ?,
		    interval_weeks = ?, start_time = ?, duration_minutes = ?, start_date = ?, end_date = ?, skip_dates = ?,
		    join_url = ?, auto_start = ?, auto_end = ?, invite_default_participants = ?, status = ?, updated_at = ?
		WHERE id = ?`,
		s.Title, s.Description, s.Kind, string(hosts), s.Timezone, string(weekdays), s.IntervalWeeks, s.StartTime,
		s.DurationMinutes, s.StartDate, s.EndDate, string(skips), s.JoinURL, boolInt(s.AutoStart), boolInt(s.AutoEnd),
		boolInt(s.InviteDefaultParticipants), s.Status, s.UpdatedAt, s.ID)
	return err
}

func (h *Handler) decodeLiveSeriesInput(w http.ResponseWriter, r *http.Request) (liveSeriesInput, bool) {
	var in liveSeriesInput
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return in, false
	}
	return in, true
}

// respondLiveSeries writes the series with its upcoming and live occurrences.
func (h *Handler) respondLiveSeries(w http.ResponseWriter, r *http.Request, status int, id string) {
	s, err := h.loadLiveSeries(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live series: reload", "error", err, "series_id", id)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	occ := []*liveEvent{}
	rows, err := h.db.QueryContext(r.Context(), liveEventSelect+`
		WHERE e.series_id = ? AND e.status IN ('scheduled', 'live') ORDER BY e.scheduled_start_at`, id)
	if err == nil {
		for rows.Next() {
			if ev, err := scanLiveEvent(rows); err == nil {
				occ = append(occ, ev)
			}
		}
		rows.Close() // #nosec G104 -- drained
	}
	h.writeJSON(w, status, map[string]any{"series": s, "occurrences": occ})
}

// CreateLiveSeries handles POST /v1/live-series.
func (h *Handler) CreateLiveSeries(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	in, ok := h.decodeLiveSeriesInput(w, r)
	if !ok {
		return
	}
	for name, missing := range map[string]bool{
		"title": in.Title == nil, "timezone": in.Timezone == nil, "weekdays": in.Weekdays == nil,
		"start_time": in.StartTime == nil, "duration_minutes": in.DurationMinutes == nil, "start_date": in.StartDate == nil,
	} {
		if missing {
			h.writeError(w, http.StatusBadRequest, name+" is required")
			return
		}
	}
	now := time.Now()
	ts := liveTime(now)
	s := &liveSeries{
		ID: uid.New(), Kind: liveEventKindOfficeHours, HostUserIDs: []string{user.ID}, IntervalWeeks: 1,
		SkipDates: []string{}, AutoStart: true, AutoEnd: true, InviteDefaultParticipants: true,
		Status: liveSeriesActive, CreatedBy: user.ID, CreatedAt: ts, UpdatedAt: ts,
	}
	if !h.applyLiveSeriesInput(w, r, user, s, in) {
		return
	}
	if err := h.saveLiveSeries(r.Context(), s, true); err != nil {
		h.logger.ErrorContext(r.Context(), "live series: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	mu := h.liveEventLock("series:" + s.ID)
	mu.Lock()
	h.materializeLiveSeries(r.Context(), s, now)
	mu.Unlock()
	h.respondLiveSeries(w, r, http.StatusCreated, s.ID)
}

// ListLiveSeries handles GET /v1/live-series?status=active|ended. Any member may read.
func (h *Handler) ListLiveSeries(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFromContext(r.Context()); !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	where, args := "", []any{}
	switch st := r.URL.Query().Get("status"); st {
	case "":
	case liveSeriesActive, liveSeriesEnded:
		where, args = ` WHERE status = ?`, []any{st}
	default:
		h.writeError(w, http.StatusBadRequest, "status must be active or ended")
		return
	}
	rows, err := h.db.QueryContext(r.Context(), liveSeriesSelect+where+` ORDER BY status, title`, args...)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live series: list", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []*liveSeries{}
	for rows.Next() {
		if s, err := scanLiveSeries(rows); err == nil {
			out = append(out, s)
		}
	}
	rows.Close() // #nosec G104 -- drained before the host lookups
	for _, s := range out {
		h.fillLiveSeriesHosts(r.Context(), s)
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"series": out})
}

func (h *Handler) loadManagedLiveSeries(w http.ResponseWriter, r *http.Request, manage bool) (*liveSeries, AuthUser, bool) {
	user, ok := userFromContext(r.Context())
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, user, false
	}
	s, err := h.loadLiveSeries(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "live series not found")
		return nil, user, false
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live series: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil, user, false
	}
	if manage && !canManageLiveSeries(user, s) {
		h.writeError(w, http.StatusForbidden, "only the creator, a host or an admin can manage this series")
		return nil, user, false
	}
	return s, user, true
}

// GetLiveSeries handles GET /v1/live-series/{id}: the series and its upcoming occurrences.
func (h *Handler) GetLiveSeries(w http.ResponseWriter, r *http.Request) {
	s, _, ok := h.loadManagedLiveSeries(w, r, false)
	if !ok {
		return
	}
	h.respondLiveSeries(w, r, http.StatusOK, s.ID)
}

// PatchLiveSeries handles PATCH /v1/live-series/{id}. The change applies to the series and
// to its future scheduled occurrences, field by field, except where an occurrence was
// edited on its own (its overrides).
func (h *Handler) PatchLiveSeries(w http.ResponseWriter, r *http.Request) {
	s, user, ok := h.loadManagedLiveSeries(w, r, true)
	if !ok {
		return
	}
	if s.Status != liveSeriesActive {
		h.writeError(w, http.StatusConflict, "an ended series cannot be edited")
		return
	}
	in, ok := h.decodeLiveSeriesInput(w, r)
	if !ok {
		return
	}
	if !h.applyLiveSeriesInput(w, r, user, s, in) {
		return
	}
	now := time.Now()
	s.UpdatedAt = liveTime(now)
	mu := h.liveEventLock("series:" + s.ID)
	mu.Lock()
	defer mu.Unlock()
	if err := h.saveLiveSeries(r.Context(), s, false); err != nil {
		h.logger.ErrorContext(r.Context(), "live series: update", "error", err, "series_id", s.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.resyncLiveSeries(r.Context(), s, now)
	h.respondLiveSeries(w, r, http.StatusOK, s.ID)
}

// EndLiveSeries handles DELETE /v1/live-series/{id}: the series stops producing
// occurrences and its future scheduled ones are cancelled. A session that is live now is
// left running, and past ones stay as history. Idempotent.
func (h *Handler) EndLiveSeries(w http.ResponseWriter, r *http.Request) {
	s, _, ok := h.loadManagedLiveSeries(w, r, true)
	if !ok {
		return
	}
	now := time.Now()
	mu := h.liveEventLock("series:" + s.ID)
	mu.Lock()
	defer mu.Unlock()
	if s.Status != liveSeriesEnded {
		s.Status, s.UpdatedAt = liveSeriesEnded, liveTime(now)
		if err := h.saveLiveSeries(r.Context(), s, false); err != nil {
			h.logger.ErrorContext(r.Context(), "live series: end", "error", err, "series_id", s.ID)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	h.cancelFutureSeriesOccurrences(r.Context(), s.ID, now)
	h.respondLiveSeries(w, r, http.StatusOK, s.ID)
}

func (h *Handler) cancelFutureSeriesOccurrences(ctx context.Context, seriesID string, now time.Time) {
	rows, err := h.db.QueryContext(ctx, liveEventSelect+` WHERE e.series_id = ? AND e.status = 'scheduled'`, seriesID)
	if err != nil {
		h.logger.Error("live series: load occurrences to cancel", "error", err, "series_id", seriesID)
		return
	}
	var evs []*liveEvent
	for rows.Next() {
		if ev, err := scanLiveEvent(rows); err == nil {
			evs = append(evs, ev)
		}
	}
	rows.Close() // #nosec G104 -- drained
	for _, ev := range evs {
		if err := h.cancelLiveEventNow(ctx, ev, now); err != nil {
			h.logger.Error("live series: cancel occurrence", "error", err, "live_event_id", ev.ID)
		}
	}
}
