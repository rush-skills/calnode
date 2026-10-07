package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/i18n"
)

// Removing a member, with an explicit choice of what happens to what they own.
//
//   - Any admin may remove any member, including other admins. The owner cannot be
//     removed (transfer ownership first) and nobody removes themselves.
//   - Upcoming meetings never block a transfer: the server moves every one the member
//     hosts (primary bookings through the reassign core, non-primary seats by cancelling
//     their calendar event and writing one on the new host's calendar) BEFORE the user
//     row goes, waiting for the provider calls - an event cannot be cancelled as a user
//     who no longer exists. A receiver who is busy at one of those times is a 409 naming
//     the meeting, and nothing has moved yet. Upcoming meetings do block "delete", since
//     bookers expect them. Past bookings never block.
//   - mode "transfer" (transfer_to = another active member): their event types (with the
//     per-event-type availability they set), the past bookings they hosted and their live
//     events move to that member, so links and history survive. A past booking the
//     receiver already hosted at the same start (idx_bookings_no_double) goes to the
//     "Former member" tombstone instead, never to a raw constraint error.
//   - mode "delete": their event types and every past booking of those event types are
//     deleted, as are the past bookings they hosted elsewhere, and their scheduled or
//     live sessions are cancelled (calendar event cancelled as them). Live events they
//     created and team-calendar share links stay, re-attributed to the admin doing the
//     removal.
//   - Their calendar connections, sessions, API keys, availability, invites and host
//     seats on other event types go with the account (ON DELETE CASCADE).

// formerMemberID is the archived, unloginable tombstone that keeps a past booking's
// host_id valid when the receiver of a transfer already hosted a booking at the same
// start. It is created on demand, never listed in the directory, never a transfer target.
// The same index applies to the tombstone itself, so when two former members hosted
// bookings at one start the second goes to "former-member-2", and so on - same rules,
// same name, numbered id and address (isFormerMember).
const (
	formerMemberID     = "former-member"
	formerMemberDomain = "calnode.invalid"
	formerMemberName   = "Former member"
	formerMemberMax    = 50
)

// isFormerMember reports whether id is the tombstone or one of its overflow siblings.
func isFormerMember(id string) bool {
	return id == formerMemberID || strings.HasPrefix(id, formerMemberID+"-")
}

// formerMemberSQL is the directory filter that hides every tombstone row.
const formerMemberSQL = `(u.id = '` + formerMemberID + `' OR u.id LIKE '` + formerMemberID + `-%')`

// removalQuerier is what the preview needs: *sql.DB outside a transaction, *sql.Tx inside
// one, so the guard that decides "may delete" is computed on the rows being deleted.
type removalQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type removalPreview struct {
	EventTypes         []string `json:"event_types"`
	UpcomingHosted     int      `json:"upcoming_hosted"`
	UpcomingOnTheirETs int      `json:"upcoming_on_their_event_types"`
	PastHosted         int      `json:"past_hosted"`
	PastOnTheirETs     int      `json:"past_on_their_event_types"`
	LiveEvents         int      `json:"live_events"`
	CalendarLinks      int      `json:"calendar_connections"`
	APIKeys            int      `json:"api_keys"`
	CanDelete          bool     `json:"can_delete"`
	CanTransfer        bool     `json:"can_transfer"`
	BlockedReason      string   `json:"blocked_reason,omitempty"`
}

// loadRemovalTarget applies the who-may-remove-whom rules and writes the error.
func (h *Handler) loadRemovalTarget(w http.ResponseWriter, r *http.Request) (AuthUser, string, bool) {
	actor, ok := userFromContext(r.Context())
	if !ok || !actor.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return AuthUser{}, "", false
	}
	targetID := r.PathValue("id")
	if targetID == actor.ID {
		h.writeError(w, http.StatusBadRequest, "you cannot remove your own account")
		return AuthUser{}, "", false
	}
	if isFormerMember(targetID) {
		// The tombstone is not a member; it exists only to keep old rows attributable.
		h.writeError(w, http.StatusNotFound, "user not found")
		return AuthUser{}, "", false
	}
	var isOwner int
	err := h.db.QueryRowContext(r.Context(), `SELECT is_owner FROM users WHERE id = ?`, targetID).Scan(&isOwner)
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "user not found")
		return AuthUser{}, "", false
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "remove user: load target", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return AuthUser{}, "", false
	}
	if isOwner != 0 {
		h.writeError(w, http.StatusBadRequest, "cannot remove the workspace owner; transfer ownership first")
		return AuthUser{}, "", false
	}
	return actor, targetID, true
}

func (h *Handler) buildRemovalPreview(ctx context.Context, q removalQuerier, targetID string) (removalPreview, error) {
	var p removalPreview
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := q.QueryContext(ctx, `SELECT name FROM event_types WHERE user_id = ? ORDER BY name`, targetID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			p.EventTypes = append(p.EventTypes, n)
		}
	}
	rows.Close() // #nosec G104 -- fully consumed above
	if p.EventTypes == nil {
		p.EventTypes = []string{}
	}
	counts := []struct {
		dst  *int
		q    string
		args []any
	}{
		{&p.UpcomingHosted, `SELECT COUNT(*) FROM bookings WHERE status != 'cancelled' AND end_at > ?
			AND (host_id = ? OR id IN (SELECT booking_id FROM booking_hosts WHERE user_id = ?))`, []any{now, targetID, targetID}},
		{&p.UpcomingOnTheirETs, `SELECT COUNT(*) FROM bookings WHERE status != 'cancelled' AND end_at > ?
			AND event_type_id IN (SELECT id FROM event_types WHERE user_id = ?)`, []any{now, targetID}},
		{&p.PastHosted, `SELECT COUNT(*) FROM bookings WHERE host_id = ? AND (status = 'cancelled' OR end_at <= ?)`, []any{targetID, now}},
		{&p.PastOnTheirETs, `SELECT COUNT(*) FROM bookings WHERE (status = 'cancelled' OR end_at <= ?)
			AND event_type_id IN (SELECT id FROM event_types WHERE user_id = ?)`, []any{now, targetID}},
		{&p.LiveEvents, `SELECT COUNT(*) FROM live_events WHERE created_by = ? OR host_user_id = ?`, []any{targetID, targetID}},
		{&p.CalendarLinks, `SELECT COUNT(*) FROM calendar_connections WHERE user_id = ?`, []any{targetID}},
		{&p.APIKeys, `SELECT COUNT(*) FROM api_keys WHERE user_id = ?`, []any{targetID}},
	}
	for _, c := range counts {
		if err := q.QueryRowContext(ctx, c.q, c.args...).Scan(c.dst); err != nil {
			return p, err
		}
	}
	// Transfer always works: upcoming meetings move to the new member with everything
	// else. Delete needs nothing upcoming, or bookers would lose meetings they expect.
	p.CanTransfer = true
	switch {
	case p.UpcomingHosted > 0:
		p.BlockedReason = fmt.Sprintf("They host %d upcoming meeting(s), so their things can only be transferred (or cancel those meetings first).", p.UpcomingHosted)
	case p.UpcomingOnTheirETs > 0:
		p.BlockedReason = fmt.Sprintf("Their event types have %d upcoming booking(s), so they can only be transferred, not deleted.", p.UpcomingOnTheirETs)
	default:
		p.CanDelete = true
	}
	return p, nil
}

// GetRemovalPreview handles GET /v1/users/{id}/removal-preview (admin).
func (h *Handler) GetRemovalPreview(w http.ResponseWriter, r *http.Request) {
	_, targetID, ok := h.loadRemovalTarget(w, r)
	if !ok {
		return
	}
	p, err := h.buildRemovalPreview(r.Context(), h.db, targetID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "remove user: preview", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, p)
}

// errRemovalConflict is a 409 the admin can act on; its text is safe for the client.
type errRemovalConflict string

func (e errRemovalConflict) Error() string { return string(e) }

// DeleteUser handles DELETE /v1/users/{id} (admin). Body (optional):
// {"mode":"transfer","transfer_to":"<user id>"} or {"mode":"delete"}. With no body, the
// mode is "delete" — which only succeeds when the member owns nothing that has history
// the caller hasn't chosen to drop (it is the old endpoint's behaviour for an empty user).
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	actor, targetID, ok := h.loadRemovalTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode       string `json:"mode"`
		TransferTo string `json:"transfer_to"`
	}
	if r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	if req.Mode == "" {
		req.Mode = "delete"
	}
	if req.Mode != "delete" && req.Mode != "transfer" {
		h.writeError(w, http.StatusBadRequest, `mode must be "transfer" or "delete"`)
		return
	}
	ctx := r.Context()
	p, err := h.buildRemovalPreview(ctx, h.db, targetID)
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: preview", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if req.Mode == "delete" && !p.CanDelete {
		h.writeError(w, http.StatusConflict, p.BlockedReason)
		return
	}
	if req.Mode == "transfer" {
		if req.TransferTo == "" || req.TransferTo == targetID || isFormerMember(req.TransferTo) {
			h.writeError(w, http.StatusBadRequest, "choose another member to transfer to")
			return
		}
		var active int
		if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ? AND archived_at IS NULL`, req.TransferTo).Scan(&active); err != nil || active == 0 {
			h.writeError(w, http.StatusBadRequest, "the member to transfer to was not found or is archived")
			return
		}
	}
	// Whoever keeps attributable rows (live events, share links): the new owner, or the
	// admin doing the removal.
	heir := actor.ID
	if req.Mode == "transfer" {
		heir = req.TransferTo
	}

	// Everything that talks to a calendar provider happens here, before the transaction:
	// the pool is a single connection, and the old host must still exist for the cancels
	// to route. A conflict is reported before anything has moved.
	if req.Mode == "transfer" {
		if err := h.transferUpcomingMeetings(ctx, targetID, req.TransferTo); err != nil {
			var conflict errRemovalConflict
			if errors.As(err, &conflict) {
				h.writeError(w, http.StatusConflict, conflict.Error())
				return
			}
			h.logger.ErrorContext(ctx, "remove user: move upcoming meetings", "error", err, "user_id", targetID)
			h.writeError(w, http.StatusInternalServerError, "could not move the member's upcoming meetings")
			return
		}
	}
	if err := h.settleLiveEventsForRemoval(ctx, targetID, req.Mode, req.TransferTo); err != nil {
		h.logger.ErrorContext(ctx, "remove user: live events", "error", err, "user_id", targetID)
		h.writeError(w, http.StatusInternalServerError, "could not settle the member's live events")
		return
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: begin", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// The guard is recomputed on the rows this transaction will act on: a booking made
	// between the preview and here must still block a delete, and in transfer mode
	// nothing upcoming may be left on the leaver (it was all moved above).
	inTx, err := h.buildRemovalPreview(ctx, tx, targetID)
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: preview in transaction", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if req.Mode == "delete" && !inTx.CanDelete {
		h.writeError(w, http.StatusConflict, inTx.BlockedReason)
		return
	}
	if req.Mode == "transfer" && inTx.UpcomingHosted > 0 {
		h.writeError(w, http.StatusConflict, "a meeting was booked with this member while they were being removed; try again")
		return
	}

	if req.Mode == "transfer" {
		err = h.transferOwnedRows(ctx, tx, targetID, req.TransferTo)
	} else {
		err = deleteOwnedRows(ctx, tx, targetID)
	}
	if err == nil {
		for _, st := range []struct {
			q    string
			args []any
		}{
			{`UPDATE live_events SET created_by = ? WHERE created_by = ?`, []any{heir, targetID}},
			{`UPDATE team_calendar_shares SET created_by = ? WHERE created_by = ?`, []any{heir, targetID}},
			{`DELETE FROM users WHERE id = ?`, []any{targetID}},
		} {
			if _, err = tx.ExecContext(ctx, st.q, st.args...); err != nil {
				break
			}
		}
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: step", "error", err, "mode", req.Mode, "user_id", targetID)
		h.writeError(w, http.StatusInternalServerError, "could not remove the member")
		return
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(ctx, "remove user: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.InfoContext(ctx, "member removed", "user_id", targetID, "by", actor.ID, "mode", req.Mode, "transfer_to", req.TransferTo,
		"event_types", len(p.EventTypes))
	h.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": req.Mode})
}

// deleteOwnedRows drops the leaver's event types with their whole booking history, and the
// past bookings they hosted elsewhere (the guard has established nothing is upcoming).
func deleteOwnedRows(ctx context.Context, tx *sql.Tx, targetID string) error {
	gone := `SELECT id FROM bookings WHERE event_type_id IN (SELECT id FROM event_types WHERE user_id = ?) OR host_id = ?`
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`UPDATE webhook_deliveries SET booking_id = NULL WHERE booking_id IN (` + gone + `)`, []any{targetID, targetID}},
		{`DELETE FROM bookings WHERE id IN (` + gone + `)`, []any{targetID, targetID}},
		{`DELETE FROM event_types WHERE user_id = ?`, []any{targetID}},
	} {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}

// transferOwnedRows re-points everything the leaver owns at the receiver: event types
// (host seats and the per-event-type availability they set travel with them), booking
// seats, the bookings they hosted - row by row, because idx_bookings_no_double forbids
// two non-cancelled bookings with one host at one start - and their live events.
func (h *Handler) transferOwnedRows(ctx context.Context, tx *sql.Tx, targetID, to string) error {
	ownedETs := `SELECT id FROM event_types WHERE user_id = ?`
	for _, st := range []struct {
		q    string
		args []any
	}{
		// Host seats on their event types pass to the new owner (dropped where the new
		// owner is already a host there).
		{`DELETE FROM event_type_hosts WHERE user_id = ? AND event_type_id IN (
			SELECT event_type_id FROM event_type_hosts WHERE user_id = ?)
			AND event_type_id IN (` + ownedETs + `)`, []any{targetID, to, targetID}},
		{`UPDATE event_type_hosts SET user_id = ? WHERE user_id = ? AND event_type_id IN (` + ownedETs + `)`, []any{to, targetID, targetID}},
		// The availability they set for those event types is part of how the event type
		// books (the same move TransferEventType makes); a rule the receiver already has
		// for that event type is dropped rather than duplicated.
		{`DELETE FROM availability_rules WHERE user_id = ? AND event_type_id IN (` + ownedETs + `)
			AND EXISTS (SELECT 1 FROM availability_rules r WHERE r.user_id = ? AND r.event_type_id = availability_rules.event_type_id
			            AND r.day_of_week = availability_rules.day_of_week AND r.start_time = availability_rules.start_time
			            AND r.end_time = availability_rules.end_time)`, []any{targetID, targetID, to}},
		{`UPDATE availability_rules SET user_id = ? WHERE user_id = ? AND event_type_id IN (` + ownedETs + `)`, []any{to, targetID, targetID}},
		{`UPDATE event_types SET user_id = ? WHERE user_id = ?`, []any{to, targetID}},
	} {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}

	// Bookings they hosted (only past and cancelled ones are left: the upcoming ones were
	// moved before this transaction). One at a time: a non-cancelled booking at a start
	// where the receiver already hosts one would violate idx_bookings_no_double, so it is
	// attributed to the tombstone instead - history stays, the constraint holds.
	rows, err := tx.QueryContext(ctx, `SELECT id, start_at, status FROM bookings WHERE host_id = ?`, targetID)
	if err != nil {
		return err
	}
	type hosted struct{ id, startAt, status string }
	var mine []hosted
	for rows.Next() {
		var b hosted
		if err := rows.Scan(&b.id, &b.startAt, &b.status); err != nil {
			rows.Close() // #nosec G104
			return err
		}
		mine = append(mine, b)
	}
	rows.Close() // #nosec G104 -- read fully before the per-row writes (single connection)
	slotTaken := func(hostID string, b hosted) (bool, error) {
		if b.status == "cancelled" {
			return false, nil // the index is partial: cancelled rows never collide
		}
		var n int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bookings WHERE host_id = ? AND start_at = ? AND status != 'cancelled'`,
			hostID, b.startAt).Scan(&n)
		return n > 0, err
	}
	for _, b := range mine {
		newHost := to
		taken, err := slotTaken(to, b)
		if err != nil {
			return err
		}
		if taken {
			if newHost, err = tombstoneFor(ctx, tx, func(id string) (bool, error) { return slotTaken(id, b) }); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE bookings SET host_id = ? WHERE id = ?`, newHost, b.id)
		if err != nil && !isFormerMember(newHost) && db.IsUniqueViolation(err) {
			// Belt and braces: the pre-check above should have caught it.
			if newHost, err = tombstoneFor(ctx, tx, func(id string) (bool, error) { return slotTaken(id, b) }); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE bookings SET host_id = ? WHERE id = ?`, newHost, b.id)
		}
		if err != nil {
			return err
		}
		if isFormerMember(newHost) {
			// Its seat follows the row, so the booking's host record stays consistent.
			if _, err := tx.ExecContext(ctx, `UPDATE booking_hosts SET user_id = ? WHERE booking_id = ? AND user_id = ?`,
				newHost, b.id, targetID); err != nil {
				return err
			}
			h.logger.InfoContext(ctx, "remove user: past booking attributed to the former-member tombstone (receiver already hosted that slot)",
				"booking_id", b.id, "user_id", targetID, "transfer_to", to, "tombstone", newHost)
		}
	}

	for _, st := range []struct {
		q    string
		args []any
	}{
		// Remaining host seats on bookings pass to the new member; where they already hold
		// a seat on that booking, the leaver's seat is simply dropped.
		{`DELETE FROM booking_hosts WHERE user_id = ? AND booking_id IN (SELECT booking_id FROM booking_hosts WHERE user_id = ?)`, []any{targetID, to}},
		{`UPDATE booking_hosts SET user_id = ? WHERE user_id = ?`, []any{to, targetID}},
		{`UPDATE live_events SET host_user_id = ? WHERE host_user_id = ?`, []any{to, targetID}},
	} {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}

// tombstoneFor returns the first tombstone user (former-member, former-member-2, …) for
// which taken reports false, creating it if needed: archived (so it is never in the
// active directory, never a host or a transfer target), no email login, an address under
// the reserved .invalid TLD so no sign-in can ever match it.
func tombstoneFor(ctx context.Context, tx *sql.Tx, taken func(id string) (bool, error)) (string, error) {
	for n := 1; n <= formerMemberMax; n++ {
		id, local := formerMemberID, formerMemberID
		if n > 1 {
			id = fmt.Sprintf("%s-%d", formerMemberID, n)
			local = id
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO users (id, email, name, iana_timezone, is_admin, is_owner, email_login, archived_at)
			VALUES (?, ?, ?, 'UTC', 0, 0, 0, ?)`,
			id, local+"@"+formerMemberDomain, formerMemberName, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return "", err
		}
		busy, err := taken(id)
		if err != nil {
			return "", err
		}
		if !busy {
			return id, nil
		}
	}
	return "", errors.New("remove user: no free former-member tombstone")
}

// upcomingSeat is one upcoming meeting the leaver is on: the booking, whether they are
// its primary host, and the calendar event stamped on their seat.
type upcomingSeat struct {
	bookingID, seatID         string
	isPrimary                 bool
	start, end                time.Time
	etName                    string
	extEventID, extCalendarID string
	extProvider               string
	receiverHasSeat           bool
}

// transferUpcomingMeetings moves every upcoming meeting the leaver hosts to the receiver,
// synchronously, before the leaver's row goes. All conflicts are checked first so a 409
// leaves nothing half-moved. Primary bookings go through the reassign core (old event
// cancelled as the leaver, a new one on the receiver's calendar, attendee and new host
// notified). A non-primary seat has the leaver's event cancelled (as them) and a fresh
// one written on the receiver's calendar; where the receiver is already on that meeting
// the seat is simply dropped.
func (h *Handler) transferUpcomingMeetings(ctx context.Context, targetID, to string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := h.db.QueryContext(ctx, `
		SELECT b.id, COALESCE(bh.id, ''), (b.host_id = ? OR COALESCE(bh.is_primary, 0) = 1),
		       b.start_at, b.end_at, et.name,
		       COALESCE(bh.external_event_id, ''), COALESCE(bh.external_calendar_id, ''), COALESCE(bh.external_provider, ''),
		       EXISTS (SELECT 1 FROM booking_hosts x WHERE x.booking_id = b.id AND x.user_id = ?)
		FROM bookings b
		JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN booking_hosts bh ON bh.booking_id = b.id AND bh.user_id = ?
		WHERE b.status != 'cancelled' AND b.end_at > ?
		  AND (b.host_id = ? OR bh.id IS NOT NULL)
		ORDER BY b.start_at ASC`, targetID, to, targetID, now, targetID)
	if err != nil {
		return err
	}
	var seats []upcomingSeat
	for rows.Next() {
		var s upcomingSeat
		var primary, has int
		var startStr, endStr string
		if err := rows.Scan(&s.bookingID, &s.seatID, &primary, &startStr, &endStr, &s.etName,
			&s.extEventID, &s.extCalendarID, &s.extProvider, &has); err != nil {
			rows.Close() // #nosec G104
			return err
		}
		s.isPrimary, s.receiverHasSeat = primary != 0, has != 0
		s.start, _ = time.Parse(time.RFC3339Nano, startStr)
		s.end, _ = time.Parse(time.RFC3339Nano, endStr)
		seats = append(seats, s)
	}
	rows.Close() // #nosec G104 -- read fully before any further query (single connection)
	if len(seats) == 0 {
		return nil
	}

	// Every conflict first, then every move.
	for _, s := range seats {
		if s.receiverHasSeat {
			continue // they are on it already; the leaver's seat will be dropped
		}
		busy, err := h.bookingSvc.HostBusy(ctx, to, s.start, s.end, s.bookingID)
		if err != nil {
			return err
		}
		if busy {
			return errRemovalConflict(fmt.Sprintf(
				"the member to transfer to already has a meeting at %s, so \"%s\" (booking %s) cannot move to them; pick someone else or cancel that meeting first",
				s.start.UTC().Format("2006-01-02 15:04 MST"), s.etName, s.bookingID))
		}
	}
	for _, s := range seats {
		if s.isPrimary {
			rb, err := h.reassignBookingRow(ctx, s.bookingID, to)
			if errors.Is(err, booking.ErrDoubleBooked) {
				return errRemovalConflict(fmt.Sprintf(
					"the member to transfer to already has a meeting at %s, so \"%s\" (booking %s) cannot move to them; pick someone else or cancel that meeting first",
					s.start.UTC().Format("2006-01-02 15:04 MST"), s.etName, s.bookingID))
			}
			if err != nil {
				return fmt.Errorf("reassign booking %s: %w", s.bookingID, err)
			}
			if s.receiverHasSeat && s.seatID != "" {
				// ReassignHost moved the primary seat onto the receiver, who already held a
				// non-primary one: keep a single seat, the primary.
				if _, err := h.db.ExecContext(ctx, `DELETE FROM booking_hosts WHERE booking_id = ? AND user_id = ? AND is_primary = 0`,
					s.bookingID, to); err != nil {
					h.logger.ErrorContext(ctx, "remove user: drop duplicate seat", "error", err, "booking_id", s.bookingID)
				}
			}
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			h.reassignSideEffects(sctx, rb)
			cancel()
			continue
		}
		if err := h.moveSecondarySeat(ctx, s, targetID, to); err != nil {
			return fmt.Errorf("move seat on booking %s: %w", s.bookingID, err)
		}
	}
	return nil
}

// moveSecondarySeat hands a non-primary host seat to the receiver: the leaver's calendar
// event is cancelled as the leaver and, unless the receiver is on the meeting already (the
// seat is then dropped), a fresh event is written on the receiver's calendar.
func (h *Handler) moveSecondarySeat(ctx context.Context, s upcomingSeat, targetID, to string) error {
	gc := h.getCal()
	if gc != nil && s.extEventID != "" {
		if err := gc.CancelEvent(ctx, targetID, s.extCalendarID, s.extEventID, s.extProvider); err != nil {
			h.logger.ErrorContext(ctx, "remove user: cancel leaver's calendar event", "error", err, "booking_id", s.bookingID, "user_id", targetID)
		}
	}
	if s.receiverHasSeat {
		_, err := h.db.ExecContext(ctx, `DELETE FROM booking_hosts WHERE id = ?`, s.seatID)
		return err
	}
	if _, err := h.db.ExecContext(ctx, `
		UPDATE booking_hosts SET user_id = ?, external_event_id = NULL, external_calendar_id = NULL, external_provider = '', needs_sync = 0
		WHERE id = ?`, to, s.seatID); err != nil {
		return err
	}
	if gc == nil {
		return nil
	}
	b, err := h.bookingSvc.Get(ctx, s.bookingID)
	if err != nil {
		return err
	}
	var orgName, orgEmail, orgLocale, calMsg string
	_ = h.db.QueryRowContext(ctx, `
		SELECT COALESCE(a.name,''), COALESCE(a.email,''), COALESCE(a.locale,''), COALESCE(et.calendar_message,'')
		FROM bookings b JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN booking_attendees a ON a.booking_id = b.id AND a.is_organizer = 1
		WHERE b.id = ?`, s.bookingID).Scan(&orgName, &orgEmail, &orgLocale, &calMsg)
	loc := i18n.Get(orgLocale)
	var extra []string
	if defaults := h.loadDefaultAttendees(ctx, "remove user"); len(defaults) > 0 {
		extra = extraAttendeesFor(defaults, append([]string{orgEmail}, h.bookingHostEmails(ctx, s.bookingID)...)...)
	}
	answers, aerr := h.loadAnswerLines(ctx, s.bookingID)
	if aerr != nil {
		h.logger.ErrorContext(ctx, "remove user: load answers for calendar event", "error", aerr, "booking_id", s.bookingID)
	}
	descPlain, descRich := calendarDescription(loc, h.withOrgCalendarMessage(ctx, calMsg), answers, h.calendarManageLinks(ctx, s.bookingID), s.bookingID)
	eventID, _, calID, provider, err := gc.CreateEvent(ctx, to, calendar.CreateEventParams{
		Summary:         loc.Tf("calendar_event_summary", s.etName, orgName),
		Description:     descPlain,
		DescriptionHTML: descRich,
		Location:        b.LocationValue, // the meeting's existing link; secondary hosts never mint
		Start:           b.StartAt,
		End:             b.EndAt,
		OrganizerName:   orgName,
		OrganizerEmail:  orgEmail,
		ExtraAttendees:  extra,
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: create receiver's calendar event", "error", err, "booking_id", s.bookingID)
		h.nudgeCalendarReconcile()
		return nil
	}
	if eventID != "" {
		if _, err := h.db.ExecContext(ctx, `
			UPDATE booking_hosts SET external_event_id = ?, external_calendar_id = ?, external_provider = ? WHERE id = ?`,
			eventID, calID, provider, s.seatID); err != nil {
			h.logger.ErrorContext(ctx, "remove user: persist receiver's event id", "error", err, "booking_id", s.bookingID)
		}
	}
	return nil
}

// settleLiveEventsForRemoval handles the leaver's scheduled and live sessions before their
// row goes: in transfer mode they are re-hosted (calendar event cancelled as the leaver, a
// fresh one - and a fresh link where the old one was minted - for the new host); in
// delete mode they are cancelled, their calendar event cancelled as the leaver.
func (h *Handler) settleLiveEventsForRemoval(ctx context.Context, targetID, mode, to string) error {
	rows, err := h.db.QueryContext(ctx, `SELECT id FROM live_events WHERE host_user_id = ? AND status IN ('scheduled', 'live')`, targetID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close() // #nosec G104 -- read fully before the per-row work (single connection)
	now := time.Now()
	for _, id := range ids {
		ev, err := h.loadLiveEvent(ctx, id)
		if err != nil {
			return err
		}
		if mode == "transfer" {
			err = h.rehostLiveEvent(ctx, ev, to, now)
		} else {
			err = h.cancelLiveEventNow(ctx, ev, now)
		}
		if err != nil {
			return fmt.Errorf("live event %s: %w", id, err)
		}
	}
	return nil
}
