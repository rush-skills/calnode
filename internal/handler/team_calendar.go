package handler

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/uid"
)

// Team calendar: one view of every member's meetings - Calnode bookings for all hosts
// plus titled events from each member's connected Google/Microsoft calendars. Read by
// any signed-in member in the admin app, and by an external dashboard through a share
// token (team_calendar_shares, migration 00074) that embeds the public page below.
//
// The share token grants read access to the whole team's schedule, including attendee
// names, and never anything else: it is accepted only by the data endpoint and the embed
// page, never by RequireAuth, so a leaked token cannot reach the API.

//go:embed templates/team-calendar.html
var teamCalendarTmplSrc string

var teamCalendarTmpl = template.Must(template.New("team-calendar").Parse(teamCalendarTmplSrc))

// teamCalendarMaxDays caps one request's window: a six-week month grid is 42 days, and
// each member with a connected calendar costs a network round-trip per request.
const teamCalendarMaxDays = 42

// teamCalendarPalette assigns each member a stable colour by index in the members list,
// which is ordered by join date so a new member never recolours the old ones.
var teamCalendarPalette = []string{
	"#2563eb", "#16a34a", "#d97706", "#dc2626", "#7c3aed",
	"#0891b2", "#db2777", "#65a30d", "#ea580c", "#4f46e5",
	"#0d9488", "#9333ea",
}

type teamCalendarMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Color     string `json:"color"`
}

type teamCalendarItem struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // "booking" | "external"
	MemberID      string `json:"member_id"`
	Title         string `json:"title"`
	Start         string `json:"start"` // RFC3339 UTC
	End           string `json:"end"`
	AllDay        bool   `json:"all_day"`
	Location      string `json:"location"`
	BookingID     string `json:"booking_id,omitempty"`
	EventTypeName string `json:"event_type_name,omitempty"`
	AttendeeName  string `json:"attendee_name,omitempty"`
	Status        string `json:"status"`
	Source        string `json:"source"` // "calnode" | "google" | "microsoft"
}

// parseTeamCalendarRange reads from/to (YYYY-MM-DD, inclusive) and returns the UTC
// window [from 00:00, to+1d 00:00). Malformed dates, a reversed range and a window over
// teamCalendarMaxDays are each a 400.
func parseTeamCalendarRange(fromStr, toStr string) (from, to time.Time, err error) {
	if fromStr == "" || toStr == "" {
		return from, to, errors.New("from and to are required (YYYY-MM-DD)")
	}
	from, err = time.Parse("2006-01-02", fromStr)
	if err != nil {
		return from, to, errors.New("from must be YYYY-MM-DD")
	}
	to, err = time.Parse("2006-01-02", toStr)
	if err != nil {
		return from, to, errors.New("to must be YYYY-MM-DD")
	}
	if to.Before(from) {
		return from, to, errors.New("to must not be before from")
	}
	if to.Sub(from) > teamCalendarMaxDays*24*time.Hour {
		return from, to, fmt.Errorf("range must be %d days or fewer", teamCalendarMaxDays)
	}
	return from.UTC(), to.UTC().Add(24 * time.Hour), nil
}

// TeamCalendar handles GET /v1/team-calendar?from=&to=[&token=].
//
// Two ways in: a session or API key (any member - the team calendar is org-visible,
// like event types), or a valid, unrevoked share token, which the embed page uses. A
// token that is present but invalid is rejected without falling through to the session,
// mirroring RequireAuth's rule for a bad API key.
func (h *Handler) TeamCalendar(w http.ResponseWriter, r *http.Request) {
	if tok := r.URL.Query().Get("token"); tok != "" {
		if ok, err := h.validTeamCalendarShare(r.Context(), tok); err != nil {
			h.logger.ErrorContext(r.Context(), "team calendar: share lookup", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		} else if !ok {
			h.writeError(w, http.StatusUnauthorized, "invalid or revoked share token")
			return
		}
		h.teamCalendarData(w, r)
		return
	}
	h.RequireAuth(h.teamCalendarData)(w, r)
}

func (h *Handler) teamCalendarData(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := parseTeamCalendarRange(q.Get("from"), q.Get("to"))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Widen the query window by a day each side: the dates are the viewer's local grid
	// and the window is UTC, so an evening meeting in Auckland on the grid's last day
	// is already the next UTC day. The client clips to its own grid.
	qFrom, qTo := from.Add(-24*time.Hour), to.Add(24*time.Hour)

	members, err := h.teamCalendarMembers(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "team calendar: members", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	memberIdx := make(map[string]int, len(members))
	for i, m := range members {
		memberIdx[m.ID] = i
	}

	items, ownEventIDs, err := h.teamCalendarBookings(r.Context(), qFrom, qTo, memberIdx)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "team calendar: bookings", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// External events, only once every DB row above has been read: each member's
	// ListEvents runs its own queries and the pool is a single connection. Only members
	// with a connection take part, so a 30-person workspace with three connected
	// calendars costs three fetches, not thirty goroutines.
	if cal := h.getCal(); cal != nil {
		connected, err := cal.ConnectedUserIDs(r.Context())
		if err != nil {
			h.logger.ErrorContext(r.Context(), "team calendar: connected users", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var withCal []teamCalendarMember
		for _, m := range members {
			if connected[m.ID] {
				withCal = append(withCal, m)
			}
		}
		items = append(items, h.teamCalendarExternal(r.Context(), cal, withCal, qFrom, qTo, ownEventIDs)...)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Start != items[j].Start {
			return items[i].Start < items[j].Start
		}
		return items[i].ID < items[j].ID
	})
	w.Header().Set("Cache-Control", "no-store")
	// Never publish a booker's phone number or a signed LiveKit join URL, and give share-token
	// viewers (an iframe outside the app) neither locations nor booking ids, which double as
	// bearer ids for GET /v1/bookings/{id}.
	viaToken := q.Get("token") != ""
	for i := range items {
		loc := items[i].Location
		if viaToken || strings.HasPrefix(loc, "tel:") || strings.Contains(loc, "/room/") {
			items[i].Location = ""
		}
		if viaToken {
			items[i].BookingID = ""
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"members": members, "items": items})
}

// teamCalendarMembers lists the active members, oldest first, each with its palette colour.
func (h *Handler) teamCalendarMembers(ctx context.Context) ([]teamCalendarMember, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(avatar_url, '') FROM users
		WHERE archived_at IS NULL
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []teamCalendarMember{}
	for rows.Next() {
		var m teamCalendarMember
		if err := rows.Scan(&m.ID, &m.Name, &m.AvatarURL); err != nil {
			return nil, err
		}
		m.Color = teamCalendarPalette[len(members)%len(teamCalendarPalette)]
		members = append(members, m)
	}
	return members, rows.Err()
}

// teamCalendarBookings returns one item per (confirmed booking, host seat) overlapping
// [from, to), plus the set of calendar event ids the platform wrote for them, which the
// external pass uses to skip our own events when they come back from the provider.
//
// Seats come from booking_hosts; a booking from before that table existed has no rows
// there and falls back to bookings.host_id, the way the visibility model reads it
// (ARCHITECTURE §14). Seats held by archived users are dropped: they are not members.
func (h *Handler) teamCalendarBookings(ctx context.Context, from, to time.Time, memberIdx map[string]int) ([]teamCalendarItem, map[string]bool, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT b.id, bh.user_id, b.start_at, b.end_at, b.status,
		       COALESCE(b.meeting_link, ''), COALESCE(b.location_value, ''),
		       COALESCE(et.name, ''),
		       COALESCE((SELECT a.name FROM booking_attendees a
		                 WHERE a.booking_id = b.id AND a.is_organizer = 1 LIMIT 1), ''),
		       COALESCE(bh.external_event_id, '')
		FROM bookings b
		JOIN booking_hosts bh ON bh.booking_id = b.id
		LEFT JOIN event_types et ON et.id = b.event_type_id
		WHERE b.status = 'confirmed' AND b.start_at < ? AND b.end_at > ?
		UNION ALL
		SELECT b.id, b.host_id, b.start_at, b.end_at, b.status,
		       COALESCE(b.meeting_link, ''), COALESCE(b.location_value, ''),
		       COALESCE(et.name, ''),
		       COALESCE((SELECT a.name FROM booking_attendees a
		                 WHERE a.booking_id = b.id AND a.is_organizer = 1 LIMIT 1), ''),
		       COALESCE(b.external_event_id, '')
		FROM bookings b
		LEFT JOIN event_types et ON et.id = b.event_type_id
		WHERE b.status = 'confirmed' AND b.start_at < ? AND b.end_at > ?
		  AND NOT EXISTS (SELECT 1 FROM booking_hosts x WHERE x.booking_id = b.id)`,
		sqlTimeTC(to), sqlTimeTC(from), sqlTimeTC(to), sqlTimeTC(from))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	items := []teamCalendarItem{}
	own := map[string]bool{}
	for rows.Next() {
		var bookingID, userID, startAt, endAt, status, link, locValue, etName, attendee, extID string
		if err := rows.Scan(&bookingID, &userID, &startAt, &endAt, &status, &link, &locValue, &etName, &attendee, &extID); err != nil {
			return nil, nil, err
		}
		if extID != "" {
			own[extID] = true
		}
		if _, ok := memberIdx[userID]; !ok {
			continue
		}
		title := etName
		if title == "" {
			title = "Booking"
		}
		if attendee != "" {
			title += " · " + attendee
		}
		loc := link
		if loc == "" {
			loc = locValue
		}
		items = append(items, teamCalendarItem{
			ID:            bookingID + ":" + userID,
			Kind:          "booking",
			MemberID:      userID,
			Title:         title,
			Start:         rfc3339UTC(startAt),
			End:           rfc3339UTC(endAt),
			Location:      loc,
			BookingID:     bookingID,
			EventTypeName: etName,
			AttendeeName:  attendee,
			Status:        status,
			Source:        "calnode",
		})
	}
	return items, own, rows.Err()
}

// teamCalendarExternal fans out ListEvents per (connected) member with a bounded
// errgroup. One member's failure (expired token, provider outage) is logged and
// whatever their other providers returned is kept - the rest of the team's calendar
// still renders. Platform-created events are dropped by id so a booking is not shown
// twice.
func (h *Handler) teamCalendarExternal(ctx context.Context, cal *calendar.Service, members []teamCalendarMember, from, to time.Time, own map[string]bool) []teamCalendarItem {
	perMember := make([][]calendar.ExternalEvent, len(members))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, m := range members {
		g.Go(func() error {
			evs, err := cal.ListEvents(gctx, m.ID, from, to)
			if err != nil {
				// Partial by design: the Service returns what the healthy providers gave.
				h.logger.WarnContext(gctx, "team calendar: external events incomplete for member", "user_id", m.ID, "error", err)
			}
			perMember[i] = evs
			return nil // never fail the response for one member
		})
	}
	_ = g.Wait() // every goroutine returns nil; the group exists for the bound

	var items []teamCalendarItem
	seen := map[string]bool{}
	for i, m := range members {
		for _, ev := range perMember[i] {
			if ev.ID == "" || own[ev.ID] {
				continue
			}
			key := m.ID + ":" + ev.ID
			if seen[key] { // the same event in two selected calendars of one member
				continue
			}
			seen[key] = true
			title := ev.Title
			if title == "" {
				title = "Busy"
			}
			items = append(items, teamCalendarItem{
				ID:       "ext:" + key,
				Kind:     "external",
				MemberID: m.ID,
				Title:    title,
				Start:    ev.Start.UTC().Format(time.RFC3339),
				End:      ev.End.UTC().Format(time.RFC3339),
				AllDay:   ev.AllDay,
				Location: ev.Location,
				Status:   "confirmed",
				Source:   ev.Source,
			})
		}
	}
	return items
}

// sqlTimeTC renders t the way bookings.start_at/end_at are stored, for a lexicographic
// comparison (same convention as booking.sqlTime).
func sqlTimeTC(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// rfc3339UTC normalises a stored timestamp to RFC3339 UTC; an unparseable value is
// passed through so the row still renders rather than vanishing.
func rfc3339UTC(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

// ----- share links -----

func hashShareToken(tok string) string { return hashAPIKey(tok) }

// validTeamCalendarShare reports whether tok matches an unrevoked share.
func (h *Handler) validTeamCalendarShare(ctx context.Context, tok string) (bool, error) {
	var id string
	err := h.db.QueryRowContext(ctx,
		`SELECT id FROM team_calendar_shares WHERE token_hash = ? AND revoked_at IS NULL`,
		hashShareToken(tok)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type teamCalendarShareJSON struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

// ListTeamCalendarShares handles GET /v1/team-calendar/shares (admin only; no tokens).
func (h *Handler) ListTeamCalendarShares(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	if !user.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT s.id, s.name, COALESCE(u.name, ''), s.created_at, s.revoked_at
		FROM team_calendar_shares s LEFT JOIN users u ON u.id = s.created_by
		ORDER BY s.created_at DESC`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list team calendar shares", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	items := []teamCalendarShareJSON{}
	for rows.Next() {
		var s teamCalendarShareJSON
		if err := rows.Scan(&s.ID, &s.Name, &s.CreatedBy, &s.CreatedAt, &s.RevokedAt); err != nil {
			h.logger.ErrorContext(r.Context(), "scan team calendar share", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list team calendar shares: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateTeamCalendarShare handles POST /v1/team-calendar/shares {name} (admin only).
// The plaintext token is returned once; only its hash is stored.
func (h *Handler) CreateTeamCalendarShare(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	if !user.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Name) > 255 {
		h.writeError(w, http.StatusBadRequest, "name must be 255 characters or fewer")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		h.logger.ErrorContext(r.Context(), "create team calendar share: rand", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	plain := "tcs_" + hex.EncodeToString(raw)
	id := uid.New()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO team_calendar_shares (id, name, token_hash, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)`, id, req.Name, hashShareToken(plain), user.ID, now); err != nil {
		h.logger.ErrorContext(r.Context(), "create team calendar share: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id":         id,
		"name":       req.Name,
		"token":      plain,
		"embed_url":  h.publicURL() + "/embed/team-calendar?token=" + plain,
		"created_at": now,
		"note":       "save this token — it will not be shown again",
	})
}

// RevokeTeamCalendarShare handles DELETE /v1/team-calendar/shares/{id} (admin only).
// Revocation sets revoked_at; the row stays so the list still shows what existed.
func (h *Handler) RevokeTeamCalendarShare(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	if !user.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := h.db.ExecContext(r.Context(),
		`UPDATE team_calendar_shares SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now, r.PathValue("id"))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "revoke team calendar share", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "share not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----- embed page -----

// teamCalendarEmbedCSP: the page is inline script + inline CSS, fetches only its own
// origin, and - unlike every other public page - MAY be framed by anyone: being put in
// an iframe on an external dashboard is its whole purpose, and it collects nothing.
// No X-Frame-Options here either; that header would override the allow.
const teamCalendarEmbedCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; font-src 'self'; frame-ancestors *"

// TeamCalendarEmbed handles GET /embed/team-calendar?token=… - the public, frameable
// page an external dashboard embeds. The token is validated here and again on every
// data fetch the page makes; a missing, unknown or revoked token is a 404 so the URL
// reveals nothing about whether a share ever existed.
func (h *Handler) TeamCalendarEmbed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", teamCalendarEmbedCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer") // the token is in this URL

	tok := r.URL.Query().Get("token")
	ok := false
	if tok != "" {
		var err error
		if ok, err = h.validTeamCalendarShare(r.Context(), tok); err != nil {
			h.logger.ErrorContext(r.Context(), "team calendar embed: share lookup", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = teamCalendarTmpl.Execute(w, map[string]any{"NotFound": true})
		return
	}
	if err := teamCalendarTmpl.Execute(w, map[string]any{"Token": tok}); err != nil {
		h.logger.ErrorContext(r.Context(), "team calendar embed: template", "error", err)
	}
}
