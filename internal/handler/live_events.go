package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/calnode/calnode/internal/richtext"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/uid"
)

// Live events / office hours: see docs/features/live-events.md.
//
// A live event is a workspace-hosted session (office hours, a webinar, a community call)
// with a lifecycle of scheduled → live → ended, or cancelled. Its join link is the host's
// calendar event's online-meeting URL (Google Meet when the host's destination calendar is
// Google, Teams when it is Microsoft) or a manual join_url override. The public status
// endpoint publishes the link only while the session is live.

const (
	liveEventKindOfficeHours = "office_hours"
	liveEventKindEvent       = "event"

	liveStatusScheduled = "scheduled"
	liveStatusLive      = "live"
	liveStatusEnded     = "ended"
	liveStatusCancelled = "cancelled"

	// liveEventDefaultDuration is the calendar block written for a session with no scheduled
	// end (start_now, or a scheduled start alone). The sweep's auto-end runs on
	// liveEventMaxRunning, not on this: the calendar block is a hint for the host's day, the
	// auto-end is a safety net against a forgotten session staying "live" overnight.
	liveEventDefaultDuration = time.Hour

	// liveEventMaxRunning is how long a live session with no scheduled end may run before
	// the sweep ends it (auto_end rows only).
	liveEventMaxRunning = 4 * time.Hour

	liveEventSweepInterval = 60 * time.Second
	liveEventTitleMax      = 200
	liveEventDescMax       = 4000
	liveEventListLimit     = 50
	liveEventListLimitMax  = 200
	liveStatusUpcomingMax  = 5
)

// errLiveNoCalendar is the 409 condition: the session has no join link and the host has
// no destination calendar that can mint one.
var errLiveNoCalendar = errors.New("live event: host has no connected calendar that can create a meeting link")

// errLiveEventGone is a transition refused because the row was cancelled (or otherwise
// left the expected state) while the caller was deciding: a cancelled session never
// becomes live or ended again, whatever was in flight. DELETE wins.
var errLiveEventGone = errors.New("live event: the session was cancelled in the meantime")

// liveEvent is one live_events row, in API shape. Nullable timestamps serialise as null.
type liveEvent struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Kind             string  `json:"kind"`
	Status           string  `json:"status"`
	HostUserID       string  `json:"host_user_id"`
	HostName         string  `json:"host_name"`
	ScheduledStartAt *string `json:"scheduled_start_at"`
	ScheduledEndAt   *string `json:"scheduled_end_at"`
	StartedAt        *string `json:"started_at"`
	EndedAt          *string `json:"ended_at"`
	AutoStart        bool    `json:"auto_start"`
	AutoEnd          bool    `json:"auto_end"`
	JoinURL          string  `json:"join_url"`
	// JoinURLMinted: the link came from the host's calendar provider (Meet/Teams), so it
	// belongs to the host's calendar event and is re-minted on a host change. A manual
	// link is kept across host changes.
	JoinURLMinted    bool   `json:"join_url_minted"`
	HasCalendarEvent bool   `json:"has_calendar_event"`
	CreatedBy        string `json:"created_by"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`

	// Stamped calendar event, never serialised: the triple routes updates and cancels to
	// the provider that wrote the event (see calendar.Service.UpdateEvent).
	externalEventID    string
	externalCalendarID string
	externalProvider   string
}

const liveEventSelect = `
	SELECT e.id, e.title, e.description, e.kind, e.status, COALESCE(e.host_user_id, ''), COALESCE(u.name, ''),
	       e.scheduled_start_at, e.scheduled_end_at, e.started_at, e.ended_at, e.auto_start, e.auto_end, e.join_url,
	       e.join_url_minted,
	       COALESCE(e.external_event_id, ''), COALESCE(e.external_calendar_id, ''), COALESCE(e.external_provider, ''),
	       e.created_by, e.created_at, e.updated_at
	FROM live_events e LEFT JOIN users u ON u.id = e.host_user_id`

type liveEventScanner interface {
	Scan(dest ...any) error
}

func scanLiveEvent(row liveEventScanner) (*liveEvent, error) {
	var ev liveEvent
	var autoStart, autoEnd, minted int
	if err := row.Scan(&ev.ID, &ev.Title, &ev.Description, &ev.Kind, &ev.Status, &ev.HostUserID, &ev.HostName,
		&ev.ScheduledStartAt, &ev.ScheduledEndAt, &ev.StartedAt, &ev.EndedAt, &autoStart, &autoEnd, &ev.JoinURL,
		&minted, &ev.externalEventID, &ev.externalCalendarID, &ev.externalProvider,
		&ev.CreatedBy, &ev.CreatedAt, &ev.UpdatedAt); err != nil {
		return nil, err
	}
	ev.AutoStart = autoStart != 0
	ev.AutoEnd = autoEnd != 0
	ev.JoinURLMinted = minted != 0
	ev.HasCalendarEvent = ev.externalEventID != ""
	return &ev, nil
}

func (h *Handler) loadLiveEvent(ctx context.Context, id string) (*liveEvent, error) {
	return scanLiveEvent(h.db.QueryRowContext(ctx, liveEventSelect+` WHERE e.id = ?`, id))
}

// liveTime formats a timestamp the way every live_events column stores it: RFC3339, UTC,
// second precision. One format everywhere keeps the string comparisons in the sweep and
// the status endpoint correct (same length, same zone, so lexical order is time order).
func liveTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

func liveTimePtr(t time.Time) *string {
	s := liveTime(t)
	return &s
}

// parseLiveTime parses a stored or submitted RFC3339 timestamp; "" and nil are "unset".
func parseLiveTime(s *string) (time.Time, bool) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*s))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// canManageLiveEvent: the creator, the host, or any admin may start, end, edit and cancel.
func canManageLiveEvent(u AuthUser, ev *liveEvent) bool {
	return u.IsAdmin || ev.CreatedBy == u.ID || (ev.HostUserID != "" && ev.HostUserID == u.ID)
}

// reLiveKind: a live-event type is any short lowercase slug ("office_hours", "demo",
// "onboarding-q4"), so a workspace can run several kinds of sessions side by side and
// filter the public feed, page and widget per kind.
var reLiveKind = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

func validLiveKind(k string) bool { return reLiveKind.MatchString(k) }

// validJoinURL accepts an absolute http(s) URL; anything else (javascript:, a bare path)
// would be rendered as a link on third-party pages by the widget.
func validJoinURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// liveEventExtraAttendees returns the guests to invite on a session's calendar event besides
// the host: the workspace's default participants (a notetaker bot, a shared mailbox) minus
// the host's own address, since the host owns the event already. A lookup failure is
// logged and yields no guests - it must never block creating the session.
func (h *Handler) liveEventExtraAttendees(ctx context.Context, hostEmail string) []string {
	src := h.defaultAttendeeEmails
	if h.liveEventAttendeeSource != nil {
		src = h.liveEventAttendeeSource
	}
	emails, err := src(ctx)
	if err != nil {
		h.logger.Error("live events: load default participants", "error", err)
		return nil
	}
	host := strings.ToLower(strings.TrimSpace(hostEmail))
	out := make([]string, 0, len(emails))
	seen := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || e == host || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// liveEventWindow is the calendar block for a session: the scheduled start (or now when
// there is none / when going live) and the scheduled end, else start + default duration.
func liveEventWindow(ev *liveEvent, now time.Time) (start, end time.Time) {
	if ev.Status == liveStatusLive {
		if t, ok := parseLiveTime(ev.StartedAt); ok {
			start = t
		}
	}
	if start.IsZero() {
		if t, ok := parseLiveTime(ev.ScheduledStartAt); ok {
			start = t
		} else {
			start = now
		}
	}
	if t, ok := parseLiveTime(ev.ScheduledEndAt); ok && t.After(start) {
		end = t
	} else {
		end = start.Add(liveEventDefaultDuration)
	}
	return start, end
}

// ensureLiveEventCalendar creates the host's calendar event for a session that has none,
// stamping the external ids and adopting the minted online-meeting link as join_url when
// the session has no manual one. It returns errLiveNoCalendar when nothing could be
// written and the session still has no join link - the caller decides whether that is a
// 409 (start, start_now) or fine for now (a scheduled session, minted at start instead).
// A session that already has a calendar event, or a manual join_url with no host calendar
// to write to, is left as is.
func (h *Handler) ensureLiveEventCalendar(ctx context.Context, ev *liveEvent, now time.Time) error {
	if ev.externalEventID != "" {
		return nil
	}
	gc := h.getCal()
	if gc == nil || ev.HostUserID == "" {
		if ev.JoinURL == "" {
			return errLiveNoCalendar
		}
		return nil
	}
	has, err := gc.HasDestination(ctx, ev.HostUserID)
	if err != nil {
		return fmt.Errorf("live event: check host calendar: %w", err)
	}
	if !has {
		if ev.JoinURL == "" {
			return errLiveNoCalendar
		}
		return nil
	}
	_, provider, err := gc.Connected(ctx, ev.HostUserID)
	if err != nil {
		return fmt.Errorf("live event: host provider: %w", err)
	}
	// Mint the provider's native online meeting (Meet on Google, Teams on Microsoft) only
	// when the session has no manual link; CalDAV cannot mint, so a CalDAV-only host must
	// supply join_url before the session can go live.
	addMeet := ev.JoinURL == "" && (providerMintsPlatform("google_meet", provider) || providerMintsPlatform("teams", provider))
	if ev.JoinURL == "" && !addMeet {
		return errLiveNoCalendar
	}
	var hostEmail string
	_ = h.db.QueryRowContext(ctx, `SELECT COALESCE(email, '') FROM users WHERE id = ?`, ev.HostUserID).Scan(&hostEmail)
	start, end := liveEventWindow(ev, now)
	eventID, link, calID, prov, err := gc.CreateEvent(ctx, ev.HostUserID, calendar.CreateEventParams{
		Summary:        ev.Title,
		Description:    liveEventDescription(ev.Description, h.orgCalendarMessage(ctx)),
		Location:       ev.JoinURL,
		Start:          start,
		End:            end,
		AddMeet:        addMeet,
		ExtraAttendees: h.liveEventExtraAttendees(ctx, hostEmail),
	})
	if err != nil {
		return fmt.Errorf("live event: create calendar event: %w", err)
	}
	if eventID == "" {
		// The destination vanished between HasDestination and CreateEvent.
		if ev.JoinURL == "" {
			return errLiveNoCalendar
		}
		return nil
	}
	ev.externalEventID, ev.externalCalendarID, ev.externalProvider = eventID, calID, prov
	ev.HasCalendarEvent = true
	if ev.JoinURL == "" {
		if link == "" {
			// The provider wrote the event but minted no link (a personal Microsoft account,
			// say). Keep the event - the host's calendar shows the session - but the session
			// cannot go live until a join_url is set.
			h.saveLiveEventCalendar(ctx, ev)
			return errLiveNoCalendar
		}
		ev.JoinURL, ev.JoinURLMinted = link, true
	}
	h.saveLiveEventCalendar(ctx, ev)
	return nil
}

func (h *Handler) saveLiveEventCalendar(ctx context.Context, ev *liveEvent) {
	if _, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET external_event_id = ?, external_calendar_id = ?, external_provider = ?,
		       join_url = ?, join_url_minted = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(ev.externalEventID), nullIfEmpty(ev.externalCalendarID), nullIfEmpty(ev.externalProvider),
		ev.JoinURL, boolInt(ev.JoinURLMinted), liveTime(time.Now()), ev.ID); err != nil {
		h.logger.Error("live events: save calendar event", "error", err, "live_event_id", ev.ID)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// syncLiveEventCalendarTime re-applies the session's window to its calendar event after a
// schedule change or at end (end = now). Provider errors are logged, never surfaced: the
// session's own state is the source of truth and the calendar is a mirror of it.
func (h *Handler) syncLiveEventCalendarTime(ctx context.Context, ev *liveEvent, start, end time.Time) {
	if ev.externalEventID == "" || ev.HostUserID == "" {
		return
	}
	gc := h.getCal()
	if gc == nil {
		return
	}
	if !end.After(start) {
		end = start.Add(time.Minute)
	}
	if err := gc.UpdateEvent(ctx, ev.HostUserID, ev.externalCalendarID, ev.externalEventID, ev.externalProvider, start, end, ""); err != nil {
		h.logger.Error("live events: update calendar event", "error", err, "live_event_id", ev.ID)
	}
}

// cancelLiveEventCalendar deletes the session's calendar event and clears the stamp.
// Provider errors are logged; the row is cleared either way so a retry cannot double-cancel.
func (h *Handler) cancelLiveEventCalendar(ctx context.Context, ev *liveEvent) {
	h.cancelLiveEventCalendarAs(ctx, ev, ev.HostUserID)
}

// cancelLiveEventCalendarAs is cancelLiveEventCalendar acting as userID: the event lives on
// the calendar of the host who held the session when it was minted, so a host change must
// cancel as that host, not the new one (whose calendar never had it).
func (h *Handler) cancelLiveEventCalendarAs(ctx context.Context, ev *liveEvent, userID string) {
	if ev.externalEventID == "" {
		return
	}
	if gc := h.getCal(); gc != nil && userID != "" {
		if err := gc.CancelEvent(ctx, userID, ev.externalCalendarID, ev.externalEventID, ev.externalProvider); err != nil {
			h.logger.Error("live events: cancel calendar event", "error", err, "live_event_id", ev.ID)
		}
	}
	ev.externalEventID, ev.externalCalendarID, ev.externalProvider = "", "", ""
	ev.HasCalendarEvent = false
	if _, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET external_event_id = NULL, external_calendar_id = NULL, external_provider = NULL,
		       updated_at = ? WHERE id = ?`, liveTime(time.Now()), ev.ID); err != nil {
		h.logger.Error("live events: clear calendar stamp", "error", err, "live_event_id", ev.ID)
	}
}

// liveEventLock returns the mutex serialising transitions of one session.
func (h *Handler) liveEventLock(id string) *sync.Mutex {
	m, _ := h.liveEventLocks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// refreshLiveEvent re-reads the row into ev (keeping the pointer the caller holds).
func (h *Handler) refreshLiveEvent(ctx context.Context, ev *liveEvent) error {
	fresh, err := h.loadLiveEvent(ctx, ev.ID)
	if err != nil {
		return err
	}
	*ev = *fresh
	return nil
}

// goLive flips a scheduled session to live at now, minting the calendar event first. The
// join link is the precondition: a session nobody can join must not be announced as live.
//
// The transition is compare-and-set. The sweep, a manual start and a cancel can all reach
// the same row within a second, and the mint in between is a provider round-trip, so:
// the per-event lock serialises starts (the second one finds the row live and returns it,
// one calendar event, not two); the status is re-read under the lock before anything is
// minted; and the write itself claims the row only from 'scheduled'. If the claim fails,
// the row changed under us while the provider was busy - the event just minted is
// cancelled again and the current row is returned: live means someone else started it
// (idempotent), anything else means it was cancelled, which wins (errLiveEventGone).
func (h *Handler) goLive(ctx context.Context, ev *liveEvent, now time.Time) error {
	mu := h.liveEventLock(ev.ID)
	mu.Lock()
	defer mu.Unlock()
	hostID := ev.HostUserID // a start may name the host; keep it across the re-read
	if err := h.refreshLiveEvent(ctx, ev); err != nil {
		return fmt.Errorf("live event: reload: %w", err)
	}
	if ev.HostUserID == "" {
		ev.HostUserID = hostID
	}
	switch ev.Status {
	case liveStatusLive:
		return nil
	case liveStatusScheduled:
	default:
		return errLiveEventGone
	}
	preLink, preMinted, preEvent := ev.JoinURL, ev.JoinURLMinted, ev.externalEventID
	if err := h.ensureLiveEventCalendar(ctx, ev, now); err != nil {
		return err
	}
	if ev.JoinURL == "" {
		// A stamped calendar event is not a join link (the provider may have written the
		// event and minted nothing, or the link was cleared since). Nobody can join, so it
		// does not go live.
		return errLiveNoCalendar
	}
	ts := liveTime(now)
	res, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET status = 'live', started_at = ?, host_user_id = ?, updated_at = ?
		WHERE id = ? AND status = 'scheduled'`,
		ts, nullIfEmpty(ev.HostUserID), ts, ev.ID)
	if err != nil {
		return fmt.Errorf("live event: go live: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost the claim: undo what the mint wrote (the event and the link it adopted),
		// then report the row as it is now.
		if ev.externalEventID != "" && ev.externalEventID != preEvent {
			h.cancelLiveEventCalendar(ctx, ev)
			ev.JoinURL, ev.JoinURLMinted = preLink, preMinted
			if _, err := h.db.ExecContext(ctx, `UPDATE live_events SET join_url = ?, join_url_minted = ? WHERE id = ?`,
				preLink, boolInt(preMinted), ev.ID); err != nil {
				h.logger.Error("live events: restore link after lost claim", "error", err, "live_event_id", ev.ID)
			}
		}
		if err := h.refreshLiveEvent(ctx, ev); err != nil {
			return fmt.Errorf("live event: reload: %w", err)
		}
		if ev.Status == liveStatusLive {
			return nil
		}
		return errLiveEventGone
	}
	ev.Status, ev.StartedAt, ev.UpdatedAt = liveStatusLive, &ts, ts
	return nil
}

// endLive flips a live session to ended at now and trims its calendar event to the real
// end, so the host's (and the notetaker's) calendar reflects what happened. Compare-and-set
// from 'live' only, under the same per-event lock as goLive: a session cancelled meanwhile
// stays cancelled (errLiveEventGone), one ended meanwhile is simply returned.
func (h *Handler) endLive(ctx context.Context, ev *liveEvent, now time.Time, syncCalendar bool) error {
	mu := h.liveEventLock(ev.ID)
	mu.Lock()
	defer mu.Unlock()
	if err := h.refreshLiveEvent(ctx, ev); err != nil {
		return fmt.Errorf("live event: reload: %w", err)
	}
	switch ev.Status {
	case liveStatusEnded:
		return nil
	case liveStatusLive:
	default:
		return errLiveEventGone
	}
	ts := liveTime(now)
	res, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET status = 'ended', ended_at = ?, updated_at = ? WHERE id = ? AND status = 'live'`, ts, ts, ev.ID)
	if err != nil {
		return fmt.Errorf("live event: end: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := h.refreshLiveEvent(ctx, ev); err != nil {
			return fmt.Errorf("live event: reload: %w", err)
		}
		if ev.Status == liveStatusEnded {
			return nil
		}
		return errLiveEventGone
	}
	start, _ := liveEventWindow(ev, now)
	ev.Status, ev.EndedAt, ev.UpdatedAt = liveStatusEnded, &ts, ts
	if syncCalendar {
		h.syncLiveEventCalendarTime(ctx, ev, start, now)
	}
	return nil
}

// rehostLiveEvent moves a scheduled or live session to newHostID. The calendar event is
// cancelled as the host who held it, a minted join link is dropped (it belonged to that
// event; a manual one is kept), the row is re-pointed, and a fresh event - with a fresh
// Meet/Teams link when needed - is minted for the new host. Used by PATCH host_user_id
// and by member removal in transfer mode. A live session keeps its link whatever its
// origin: the people already in the room must not be moved mid-session.
func (h *Handler) rehostLiveEvent(ctx context.Context, ev *liveEvent, newHostID string, now time.Time) error {
	oldHost := ev.HostUserID
	h.cancelLiveEventCalendarAs(ctx, ev, oldHost)
	ev.HostUserID = newHostID
	if ev.JoinURLMinted && ev.Status == liveStatusScheduled {
		ev.JoinURL, ev.JoinURLMinted = "", false
	}
	ev.UpdatedAt = liveTime(now)
	if _, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET host_user_id = ?, join_url = ?, join_url_minted = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(ev.HostUserID), ev.JoinURL, boolInt(ev.JoinURLMinted), ev.UpdatedAt, ev.ID); err != nil {
		return fmt.Errorf("live event: rehost: %w", err)
	}
	if err := h.ensureLiveEventCalendar(ctx, ev, now); err != nil && !errors.Is(err, errLiveNoCalendar) {
		return err
	}
	return nil
}

// cancelLiveEventNow is the cancel transition (a live session is ended first, its calendar
// event is deleted) shared by DELETE and by member removal. Idempotent; the status write
// never resurrects a cancelled row.
func (h *Handler) cancelLiveEventNow(ctx context.Context, ev *liveEvent, now time.Time) error {
	if ev.Status == liveStatusCancelled {
		return nil
	}
	if ev.Status == liveStatusLive {
		// No calendar sync here: cancelLiveEventCalendar deletes the event right after,
		// so trimming its end first would be a wasted provider round-trip.
		if err := h.endLive(ctx, ev, now, false); err != nil && !errors.Is(err, errLiveEventGone) {
			return err
		}
	}
	if _, err := h.db.ExecContext(ctx, `
		UPDATE live_events SET status = 'cancelled', updated_at = ? WHERE id = ? AND status != 'cancelled'`, liveTime(now), ev.ID); err != nil {
		return fmt.Errorf("live event: cancel: %w", err)
	}
	ev.Status = liveStatusCancelled
	h.cancelLiveEventCalendar(ctx, ev)
	return nil
}

// ---------------------------------------------------------------------------
// Authenticated API
// ---------------------------------------------------------------------------

type liveEventInput struct {
	Title            *string `json:"title"`
	Description      *string `json:"description"`
	Kind             *string `json:"kind"`
	ScheduledStartAt *string `json:"scheduled_start_at"`
	ScheduledEndAt   *string `json:"scheduled_end_at"`
	StartNow         bool    `json:"start_now"`
	HostUserID       *string `json:"host_user_id"`
	JoinURL          *string `json:"join_url"`
	AutoStart        *bool   `json:"auto_start"`
	AutoEnd          *bool   `json:"auto_end"`
}

func (h *Handler) decodeLiveEventInput(w http.ResponseWriter, r *http.Request) (liveEventInput, bool) {
	var in liveEventInput
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return in, false
	}
	return in, true
}

// resolveLiveHost validates a requested host: admins may name any active member; everyone
// else may only host themselves. Returns ("", false) after writing the error.
func (h *Handler) resolveLiveHost(w http.ResponseWriter, r *http.Request, caller AuthUser, requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == caller.ID {
		return caller.ID, true
	}
	if !caller.IsAdmin {
		h.writeError(w, http.StatusForbidden, "only admins can assign another host")
		return "", false
	}
	var n int
	if err := h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE id = ? AND archived_at IS NULL`, requested).Scan(&n); err != nil || n == 0 {
		h.writeError(w, http.StatusBadRequest, "host_user_id is not an active member")
		return "", false
	}
	return requested, true
}

// validateLiveSchedule parses the optional window, requiring end > start when both are set.
func (h *Handler) validateLiveSchedule(w http.ResponseWriter, startIn, endIn *string) (start, end *string, ok bool) {
	if startIn != nil && strings.TrimSpace(*startIn) != "" {
		t, parsed := parseLiveTime(startIn)
		if !parsed {
			h.writeError(w, http.StatusBadRequest, "scheduled_start_at must be RFC3339")
			return nil, nil, false
		}
		start = liveTimePtr(t)
	}
	if endIn != nil && strings.TrimSpace(*endIn) != "" {
		t, parsed := parseLiveTime(endIn)
		if !parsed {
			h.writeError(w, http.StatusBadRequest, "scheduled_end_at must be RFC3339")
			return nil, nil, false
		}
		end = liveTimePtr(t)
	}
	if start != nil && end != nil && *end <= *start {
		h.writeError(w, http.StatusBadRequest, "scheduled_end_at must be after scheduled_start_at")
		return nil, nil, false
	}
	return start, end, true
}

// CreateLiveEvent handles POST /v1/live-events.
func (h *Handler) CreateLiveEvent(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	in, ok := h.decodeLiveEventInput(w, r)
	if !ok {
		return
	}
	title := ""
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if title == "" {
		h.writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > liveEventTitleMax {
		h.writeError(w, http.StatusBadRequest, "title is too long")
		return
	}
	desc := ""
	if in.Description != nil {
		desc = strings.TrimSpace(*in.Description)
		if len(desc) > liveEventDescMax {
			h.writeError(w, http.StatusBadRequest, "description is too long")
			return
		}
	}
	kind := liveEventKindOfficeHours
	if in.Kind != nil && strings.TrimSpace(*in.Kind) != "" {
		kind = strings.TrimSpace(*in.Kind)
		if !validLiveKind(kind) {
			h.writeError(w, http.StatusBadRequest, "kind must be a short lowercase name: letters, digits, - or _ (e.g. office_hours, demo)")
			return
		}
	}
	joinURL := ""
	if in.JoinURL != nil && strings.TrimSpace(*in.JoinURL) != "" {
		joinURL = strings.TrimSpace(*in.JoinURL)
		if !validJoinURL(joinURL) {
			h.writeError(w, http.StatusBadRequest, "join_url must be an http(s) URL")
			return
		}
	}
	start, end, ok := h.validateLiveSchedule(w, in.ScheduledStartAt, in.ScheduledEndAt)
	if !ok {
		return
	}
	hostID := ""
	if in.HostUserID != nil {
		hostID = *in.HostUserID
	}
	hostID, ok = h.resolveLiveHost(w, r, user, hostID)
	if !ok {
		return
	}
	autoStart, autoEnd := true, true
	if in.AutoStart != nil {
		autoStart = *in.AutoStart
	}
	if in.AutoEnd != nil {
		autoEnd = *in.AutoEnd
	}

	now := time.Now()
	ts := liveTime(now)
	if in.StartNow && end != nil && *end <= ts {
		h.writeError(w, http.StatusBadRequest, "scheduled_end_at must be in the future when starting now")
		return
	}
	ev := &liveEvent{
		ID: uid.New(), Title: title, Description: desc, Kind: kind, Status: liveStatusScheduled,
		HostUserID: hostID, ScheduledStartAt: start, ScheduledEndAt: end,
		AutoStart: autoStart, AutoEnd: autoEnd, JoinURL: joinURL,
		CreatedBy: user.ID, CreatedAt: ts, UpdatedAt: ts,
	}
	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO live_events (id, title, description, kind, status, host_user_id, scheduled_start_at, scheduled_end_at,
		                         auto_start, auto_end, join_url, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'scheduled', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Title, ev.Description, ev.Kind, ev.HostUserID, ev.ScheduledStartAt, ev.ScheduledEndAt,
		boolInt(ev.AutoStart), boolInt(ev.AutoEnd), ev.JoinURL, ev.CreatedBy, ev.CreatedAt, ev.UpdatedAt); err != nil {
		h.logger.ErrorContext(r.Context(), "live events: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if in.StartNow {
		if err := h.goLive(r.Context(), ev, now); err != nil {
			// A failed start_now leaves nothing behind: the 409 is actionable (set a
			// join_url, connect a calendar) and an external service retrying the same
			// request must not pile up scheduled rows it never asked for.
			h.cancelLiveEventCalendar(r.Context(), ev)
			if _, derr := h.db.ExecContext(r.Context(), `DELETE FROM live_events WHERE id = ?`, ev.ID); derr != nil {
				h.logger.ErrorContext(r.Context(), "live events: roll back failed start_now", "error", derr, "live_event_id", ev.ID)
			}
			h.writeLiveEventError(w, r, ev, err)
			return
		}
	} else if err := h.ensureLiveEventCalendar(r.Context(), ev, now); err != nil && !errors.Is(err, errLiveNoCalendar) {
		// A scheduled session may exist without a calendar event (minted at start); only a
		// provider failure is worth reporting, and even then the session was created.
		h.logger.ErrorContext(r.Context(), "live events: calendar event at create", "error", err, "live_event_id", ev.ID)
	}
	h.respondLiveEvent(w, r, http.StatusCreated, ev.ID)
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// writeLiveEventError maps a go-live failure: the no-link case is a 409 the caller can act
// on; anything else is a provider/database failure.
func (h *Handler) writeLiveEventError(w http.ResponseWriter, r *http.Request, ev *liveEvent, err error) {
	if errors.Is(err, errLiveNoCalendar) {
		h.writeError(w, http.StatusConflict, "the host has no connected calendar that can create a meeting link; connect a Google or Microsoft calendar or set join_url")
		return
	}
	if errors.Is(err, errLiveEventGone) {
		h.writeError(w, http.StatusConflict, "this live event was cancelled in the meantime")
		return
	}
	h.logger.ErrorContext(r.Context(), "live events: start", "error", err, "live_event_id", ev.ID)
	h.writeError(w, http.StatusBadGateway, "could not create the calendar event for this session")
}

// respondLiveEvent re-reads the row (for host_name and the stamped fields) and writes it.
func (h *Handler) respondLiveEvent(w http.ResponseWriter, r *http.Request, status int, id string) {
	ev, err := h.loadLiveEvent(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live events: reload", "error", err, "live_event_id", id)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, status, ev)
}

// ListLiveEvents handles GET /v1/live-events?status=&limit=. Order: live first (most
// recently started first), then upcoming scheduled by start (unscheduled last), then the
// rest most recent first.
func (h *Handler) ListLiveEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFromContext(r.Context()); !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	q := r.URL.Query()
	limit := liveEventListLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			h.writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > liveEventListLimitMax {
			n = liveEventListLimitMax
		}
		limit = n
	}
	// status: one of the four states, or "history" for ended + cancelled. kind filters to
	// one session type. offset pages through history.
	conds := []string{}
	args := []any{}
	if st := q.Get("status"); st != "" {
		switch st {
		case liveStatusScheduled, liveStatusLive, liveStatusEnded, liveStatusCancelled:
			conds = append(conds, `e.status = ?`)
			args = append(args, st)
		case "history":
			conds = append(conds, `e.status IN ('ended','cancelled')`)
		case "active":
			conds = append(conds, `e.status IN ('scheduled','live')`)
		default:
			h.writeError(w, http.StatusBadRequest, "status must be scheduled, live, ended, cancelled, active or history")
			return
		}
	}
	if k := strings.TrimSpace(q.Get("kind")); k != "" {
		if !validLiveKind(k) {
			h.writeError(w, http.StatusBadRequest, "invalid kind")
			return
		}
		conds = append(conds, `e.kind = ?`)
		args = append(args, k)
	}
	offset := 0
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			h.writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}
	where := ""
	if len(conds) > 0 {
		where = ` WHERE ` + strings.Join(conds, ` AND `)
	}
	args = append(args, limit, offset)
	rows, err := h.db.QueryContext(r.Context(), liveEventSelect+where+`
		ORDER BY CASE e.status WHEN 'live' THEN 0 WHEN 'scheduled' THEN 1 WHEN 'ended' THEN 2 ELSE 3 END,
		         CASE WHEN e.status = 'live' THEN e.started_at END DESC,
		         CASE WHEN e.status = 'scheduled' THEN COALESCE(e.scheduled_start_at, '9999') END ASC,
		         COALESCE(e.ended_at, e.updated_at) DESC
		LIMIT ? OFFSET ?`, args...) // #nosec G202 -- conds are fixed literals; values are bound
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live events: list", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []*liveEvent{}
	for rows.Next() {
		ev, err := scanLiveEvent(rows)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "live events: scan", "error", err)
			continue
		}
		out = append(out, ev)
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"live_events": out})
}

// loadManagedLiveEvent resolves {id} and the caller's right to act on it, writing 404 / 403.
func (h *Handler) loadManagedLiveEvent(w http.ResponseWriter, r *http.Request) (*liveEvent, AuthUser, bool) {
	user, ok := userFromContext(r.Context())
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, user, false
	}
	ev, err := h.loadLiveEvent(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "live event not found")
		return nil, user, false
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live events: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil, user, false
	}
	if !canManageLiveEvent(user, ev) {
		h.writeError(w, http.StatusForbidden, "only the creator, the host or an admin can manage this live event")
		return nil, user, false
	}
	return ev, user, true
}

// GetLiveEvent handles GET /v1/live-events/{id}. Any member may read.
func (h *Handler) GetLiveEvent(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFromContext(r.Context()); !ok {
		h.writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ev, err := h.loadLiveEvent(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "live event not found")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live events: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, ev)
}

// PatchLiveEvent handles PATCH /v1/live-events/{id}. Fields are validated on change only:
// an omitted field keeps its stored value and is never re-validated.
func (h *Handler) PatchLiveEvent(w http.ResponseWriter, r *http.Request) {
	ev, user, ok := h.loadManagedLiveEvent(w, r)
	if !ok {
		return
	}
	if ev.Status == liveStatusEnded || ev.Status == liveStatusCancelled {
		h.writeError(w, http.StatusConflict, "an ended or cancelled live event cannot be edited")
		return
	}
	in, ok := h.decodeLiveEventInput(w, r)
	if !ok {
		return
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || len(t) > liveEventTitleMax {
			h.writeError(w, http.StatusBadRequest, "title is required and must be at most 200 characters")
			return
		}
		ev.Title = t
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if len(d) > liveEventDescMax {
			h.writeError(w, http.StatusBadRequest, "description is too long")
			return
		}
		ev.Description = d
	}
	if in.Kind != nil {
		k := strings.TrimSpace(*in.Kind)
		if !validLiveKind(k) {
			h.writeError(w, http.StatusBadRequest, "kind must be a short lowercase name: letters, digits, - or _ (e.g. office_hours, demo)")
			return
		}
		ev.Kind = k
	}
	linkChanged := false
	if in.JoinURL != nil {
		j := strings.TrimSpace(*in.JoinURL)
		if j != "" && !validJoinURL(j) {
			h.writeError(w, http.StatusBadRequest, "join_url must be an http(s) URL")
			return
		}
		if j == "" && ev.Status == liveStatusLive {
			h.writeError(w, http.StatusBadRequest, "a live session needs a join_url; end it first")
			return
		}
		linkChanged = j != ev.JoinURL
		ev.JoinURL = j
		if linkChanged {
			ev.JoinURLMinted = false // typed in (or cleared): no longer the provider's
		}
	}
	scheduleChanged := false
	if in.ScheduledStartAt != nil || in.ScheduledEndAt != nil {
		startIn, endIn := ev.ScheduledStartAt, ev.ScheduledEndAt
		if in.ScheduledStartAt != nil {
			startIn = in.ScheduledStartAt
		}
		if in.ScheduledEndAt != nil {
			endIn = in.ScheduledEndAt
		}
		start, end, ok := h.validateLiveSchedule(w, startIn, endIn)
		if !ok {
			return
		}
		ev.ScheduledStartAt, ev.ScheduledEndAt = start, end
		scheduleChanged = true
	}
	hostChanged := false
	newHostID := ev.HostUserID
	if requested := strings.TrimSpace(ptrString(in.HostUserID)); requested != "" && requested != ev.HostUserID {
		// "" means "no change": clients that echo the create payload send it, and reading
		// it as "default to the caller" would silently reassign someone else's session.
		if ev.Status == liveStatusLive {
			h.writeError(w, http.StatusConflict, "the host of a live session cannot be changed; end it and start a new one")
			return
		}
		hostID, ok := h.resolveLiveHost(w, r, user, requested)
		if !ok {
			return
		}
		// Not applied to ev yet: the old host is needed to cancel the calendar event
		// that lives on their calendar (rehostLiveEvent).
		newHostID = hostID
		hostChanged = true
	}
	if in.AutoStart != nil {
		ev.AutoStart = *in.AutoStart
	}
	if in.AutoEnd != nil {
		ev.AutoEnd = *in.AutoEnd
	}

	now := time.Now()
	ev.UpdatedAt = liveTime(now)
	if _, err := h.db.ExecContext(r.Context(), `
		UPDATE live_events SET title = ?, description = ?, kind = ?, scheduled_start_at = ?, scheduled_end_at = ?,
		       auto_start = ?, auto_end = ?, join_url = ?, join_url_minted = ?, updated_at = ? WHERE id = ?`,
		ev.Title, ev.Description, ev.Kind, ev.ScheduledStartAt, ev.ScheduledEndAt,
		boolInt(ev.AutoStart), boolInt(ev.AutoEnd), ev.JoinURL, boolInt(ev.JoinURLMinted), ev.UpdatedAt, ev.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "live events: update", "error", err, "live_event_id", ev.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The calendar event lives on the host's calendar: a new host means the old event is
	// cancelled (as the old host) and a fresh one minted for the new one, with a fresh
	// link when the old one was minted (rehostLiveEvent). A schedule change on the same
	// host just moves the existing event.
	// A changed join link on a scheduled session re-creates the event too: the conference
	// on the old event would otherwise send the host and the notetaker into a different
	// room than the one the public feed publishes. (Only scheduled: a live session cannot
	// change host, and its link may only be replaced by another manual one.)
	if hostChanged {
		if err := h.rehostLiveEvent(r.Context(), ev, newHostID, now); err != nil {
			h.logger.ErrorContext(r.Context(), "live events: calendar event after host change", "error", err, "live_event_id", ev.ID)
		}
	} else if linkChanged && ev.Status == liveStatusScheduled && ev.externalEventID != "" {
		h.cancelLiveEventCalendar(r.Context(), ev)
		if err := h.ensureLiveEventCalendar(r.Context(), ev, now); err != nil && !errors.Is(err, errLiveNoCalendar) {
			h.logger.ErrorContext(r.Context(), "live events: calendar event after link change", "error", err, "live_event_id", ev.ID)
		}
	} else if scheduleChanged && ev.Status == liveStatusScheduled {
		start, end := liveEventWindow(ev, now)
		h.syncLiveEventCalendarTime(r.Context(), ev, start, end)
	}
	h.respondLiveEvent(w, r, http.StatusOK, ev.ID)
}

// StartLiveEvent handles POST /v1/live-events/{id}/start. Idempotent on a live row.
func (h *Handler) StartLiveEvent(w http.ResponseWriter, r *http.Request) {
	ev, user, ok := h.loadManagedLiveEvent(w, r)
	if !ok {
		return
	}
	switch ev.Status {
	case liveStatusLive:
		h.writeJSON(w, http.StatusOK, ev)
		return
	case liveStatusEnded, liveStatusCancelled:
		h.writeError(w, http.StatusConflict, "an ended or cancelled live event cannot be started")
		return
	}
	// Host: the stored one; else host_user_id from the body (admins); else the caller.
	if ev.HostUserID == "" {
		requested := ""
		if r.ContentLength != 0 {
			var in struct {
				HostUserID string `json:"host_user_id"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				h.writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			requested = in.HostUserID
		}
		hostID, ok := h.resolveLiveHost(w, r, user, requested)
		if !ok {
			return
		}
		ev.HostUserID = hostID
	}
	if err := h.goLive(r.Context(), ev, time.Now()); err != nil {
		h.writeLiveEventError(w, r, ev, err)
		return
	}
	h.respondLiveEvent(w, r, http.StatusOK, ev.ID)
}

// EndLiveEvent handles POST /v1/live-events/{id}/end. Idempotent on an ended row; a
// scheduled session that never went live is a 409 (cancel it instead).
func (h *Handler) EndLiveEvent(w http.ResponseWriter, r *http.Request) {
	ev, _, ok := h.loadManagedLiveEvent(w, r)
	if !ok {
		return
	}
	switch ev.Status {
	case liveStatusEnded:
		h.writeJSON(w, http.StatusOK, ev)
		return
	case liveStatusScheduled, liveStatusCancelled:
		h.writeError(w, http.StatusConflict, "only a live session can be ended")
		return
	}
	if err := h.endLive(r.Context(), ev, time.Now(), true); err != nil {
		if errors.Is(err, errLiveEventGone) {
			h.writeError(w, http.StatusConflict, "only a live session can be ended")
			return
		}
		h.logger.ErrorContext(r.Context(), "live events: end", "error", err, "live_event_id", ev.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.respondLiveEvent(w, r, http.StatusOK, ev.ID)
}

// CancelLiveEvent handles DELETE /v1/live-events/{id}: the row is kept as cancelled (a
// live one is ended first) and its calendar event is deleted. Idempotent.
func (h *Handler) CancelLiveEvent(w http.ResponseWriter, r *http.Request) {
	ev, _, ok := h.loadManagedLiveEvent(w, r)
	if !ok {
		return
	}
	if ev.Status == liveStatusCancelled {
		h.writeJSON(w, http.StatusOK, ev)
		return
	}
	if err := h.cancelLiveEventNow(r.Context(), ev, time.Now()); err != nil {
		h.logger.ErrorContext(r.Context(), "live events: cancel", "error", err, "live_event_id", ev.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.respondLiveEvent(w, r, http.StatusOK, ev.ID)
}

// ---------------------------------------------------------------------------
// Public status
// ---------------------------------------------------------------------------

type livePublicSession struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Description      string  `json:"description"`
	Kind             string  `json:"kind"`
	HostName         string  `json:"host_name"`
	JoinURL          string  `json:"join_url,omitempty"`
	StartedAt        *string `json:"started_at,omitempty"`
	ScheduledStartAt *string `json:"scheduled_start_at,omitempty"`
	ScheduledEndAt   *string `json:"scheduled_end_at,omitempty"`
}

// LiveStatus handles GET /v1/live/status?kind= (public, no auth). It publishes the join
// link of live sessions only; scheduled ones expose their window and nothing more. The
// response is CORS-open and uncacheable so the widget on any site always sees the truth.
func (h *Handler) LiveStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && !validLiveKind(kind) {
		h.writeError(w, http.StatusBadRequest, "kind must be a short lowercase name: letters, digits, - or _ (e.g. office_hours, demo)")
		return
	}
	now := liveTime(time.Now())
	kindFilter, args := "", []any{}
	if kind != "" {
		kindFilter = ` AND e.kind = ?`
		args = append(args, kind)
	}

	// A session has a single host, and single-host events always show their host.
	hostName := func(n string) string { return n }
	live := []livePublicSession{}
	rows, err := h.db.QueryContext(r.Context(), liveEventSelect+` WHERE e.status = 'live'`+kindFilter+` ORDER BY e.started_at DESC`, args...)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live status: live rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for rows.Next() {
		if ev, err := scanLiveEvent(rows); err == nil {
			live = append(live, livePublicSession{
				ID: ev.ID, Title: ev.Title, Description: ev.Description, Kind: ev.Kind, HostName: hostName(ev.HostName),
				JoinURL: ev.JoinURL, StartedAt: ev.StartedAt,
			})
		}
	}
	rows.Close() // #nosec G104 -- rows fully consumed; the next query needs the single connection

	// Scheduled sessions whose window has not fully passed: a session that is past its
	// start but not yet live (auto_start off, host running late) is still "next" until its
	// scheduled end, or for liveEventMaxRunning after its start when it has none.
	openEndedCutoff := liveTime(time.Now().Add(-liveEventMaxRunning))
	upcoming := []livePublicSession{}
	rows, err = h.db.QueryContext(r.Context(), liveEventSelect+`
		WHERE e.status = 'scheduled' AND e.scheduled_start_at IS NOT NULL
		  AND ((e.scheduled_end_at IS NOT NULL AND e.scheduled_end_at >= ?)
		    OR (e.scheduled_end_at IS NULL AND e.scheduled_start_at >= ?))`+kindFilter+`
		ORDER BY e.scheduled_start_at ASC LIMIT ?`, append(append([]any{now, openEndedCutoff}, args...), liveStatusUpcomingMax+1)...)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "live status: upcoming rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	for rows.Next() {
		if ev, err := scanLiveEvent(rows); err == nil {
			upcoming = append(upcoming, livePublicSession{
				ID: ev.ID, Title: ev.Title, Description: ev.Description, Kind: ev.Kind, HostName: hostName(ev.HostName),
				ScheduledStartAt: ev.ScheduledStartAt, ScheduledEndAt: ev.ScheduledEndAt,
			})
		}
	}
	var next *livePublicSession
	if len(upcoming) > 0 {
		next = &upcoming[0]
		upcoming = upcoming[1:]
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"live": live, "next": next, "upcoming": upcoming})
}

// ---------------------------------------------------------------------------
// Sweep: auto-start and auto-end
// ---------------------------------------------------------------------------

// StartLiveEventSweeper runs SweepLiveEvents every minute until ctx is cancelled, plus one
// pass at startup so a restart never leaves a session stuck past its window.
func (h *Handler) StartLiveEventSweeper(ctx context.Context) {
	go func() {
		h.SweepLiveEvents(ctx, time.Now())
		ticker := time.NewTicker(liveEventSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.SweepLiveEvents(ctx, time.Now())
			}
		}
	}()
}

// SweepLiveEvents applies the automatic transitions as of now: auto_start sessions whose
// scheduled start has passed go live; auto_end live sessions past their scheduled end (or,
// with none, liveEventMaxRunning after they started) are ended. Exported with an explicit
// clock so tests drive it deterministically.
func (h *Handler) SweepLiveEvents(ctx context.Context, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	nowStr := liveTime(now)

	// Read fully before acting: the single SQLite connection cannot serve the per-row
	// updates while a cursor is open.
	collect := func(query string, args ...any) []string {
		rows, err := h.db.QueryContext(ctx, query, args...)
		if err != nil {
			h.logger.Error("live events sweep: query", "error", err)
			return nil
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		return ids
	}

	// Stale rows are never auto-started: a session whose scheduled end has passed, or an
	// open-ended one scheduled more than liveEventMaxRunning ago (a forgotten entry, a
	// workspace that was down for days), would otherwise go live the moment the sweep
	// first sees it and publish a link to a meeting nobody is holding. auto_start is
	// turned off so the host decides, once, by hand.
	staleCutoff := liveTime(now.Add(-liveEventMaxRunning))
	for _, id := range collect(`
		SELECT id FROM live_events WHERE status = 'scheduled' AND auto_start = 1
		  AND scheduled_start_at IS NOT NULL AND scheduled_start_at <= ?
		  AND ((scheduled_end_at IS NOT NULL AND scheduled_end_at <= ?) OR scheduled_start_at < ?)`, nowStr, nowStr, staleCutoff) {
		h.logger.Warn("live events sweep: auto-start skipped, scheduled start is stale; auto_start turned off", "live_event_id", id)
		if _, err := h.db.ExecContext(ctx, `UPDATE live_events SET auto_start = 0, updated_at = ? WHERE id = ?`, nowStr, id); err != nil {
			h.logger.Error("live events sweep: disable auto_start", "error", err, "live_event_id", id)
		}
	}

	for _, id := range collect(`
		SELECT id FROM live_events WHERE status = 'scheduled' AND auto_start = 1
		  AND scheduled_start_at IS NOT NULL AND scheduled_start_at <= ? AND scheduled_start_at >= ?
		  AND (scheduled_end_at IS NULL OR scheduled_end_at > ?)`, nowStr, staleCutoff, nowStr) {
		ev, err := h.loadLiveEvent(ctx, id)
		if err != nil {
			continue
		}
		if err := h.goLive(ctx, ev, now); err != nil {
			if errors.Is(err, errLiveEventGone) {
				continue // cancelled under us; DELETE wins
			}
			if errors.Is(err, errLiveNoCalendar) {
				// Nobody could join, so announcing it live would be wrong, and retrying every
				// minute changes nothing. Hand it back to the host: auto_start off, start it
				// by hand once a join link exists.
				h.logger.Warn("live events sweep: auto-start skipped, no join link; auto_start turned off", "live_event_id", id)
				if _, err := h.db.ExecContext(ctx, `UPDATE live_events SET auto_start = 0, updated_at = ? WHERE id = ?`, nowStr, id); err != nil {
					h.logger.Error("live events sweep: disable auto_start", "error", err, "live_event_id", id)
				}
				continue
			}
			h.logger.Error("live events sweep: auto-start", "error", err, "live_event_id", id)
		}
	}

	maxRunningCutoff := liveTime(now.Add(-liveEventMaxRunning))
	for _, id := range collect(`
		SELECT id FROM live_events WHERE status = 'live' AND auto_end = 1
		  AND ((scheduled_end_at IS NOT NULL AND scheduled_end_at <= ?)
		    OR (scheduled_end_at IS NULL AND started_at IS NOT NULL AND started_at <= ?))`, nowStr, maxRunningCutoff) {
		ev, err := h.loadLiveEvent(ctx, id)
		if err != nil {
			continue
		}
		if err := h.endLive(ctx, ev, now, true); err != nil && !errors.Is(err, errLiveEventGone) {
			h.logger.Error("live events sweep: auto-end", "error", err, "live_event_id", id)
		}
	}
}

// liveEventDescription is the session description followed by the workspace's default
// invite message (as plain text), so live events carry the same notice as bookings.
func liveEventDescription(desc, orgMessageHTML string) string {
	org := strings.TrimSpace(richtext.ToPlainText(orgMessageHTML))
	desc = strings.TrimSpace(desc)
	switch {
	case org == "":
		return desc
	case desc == "":
		return org
	default:
		return desc + "\n\n" + org
	}
}
