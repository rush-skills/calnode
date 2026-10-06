package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Removing a member, with an explicit choice of what happens to what they own.
//
//   - Any admin may remove any member, including other admins. The owner cannot be
//     removed (transfer ownership first) and nobody removes themselves.
//   - Upcoming meetings never block a transfer: they move to the new member (the
//     Members page reassigns primary bookings through the reassign flow first, so the
//     calendar event moves too). They do block "delete", since bookers expect them.
//     Past bookings never block.
//   - mode "transfer" (transfer_to = another active member): their event types, the
//     past bookings they hosted and their live events move to that member, so links
//     and history survive.
//   - mode "delete": their event types and every past booking of those event types are
//     deleted, as are the past bookings they hosted elsewhere. Live events they created
//     and team-calendar share links stay, re-attributed to the admin doing the removal.
//   - Their calendar connections, sessions, API keys, availability, invites and host
//     seats on other event types go with the account (ON DELETE CASCADE).

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

func (h *Handler) buildRemovalPreview(ctx context.Context, targetID string) (removalPreview, error) {
	var p removalPreview
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := h.db.QueryContext(ctx, `SELECT name FROM event_types WHERE user_id = ? ORDER BY name`, targetID)
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
		if err := h.db.QueryRowContext(ctx, c.q, c.args...).Scan(c.dst); err != nil {
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
	p, err := h.buildRemovalPreview(r.Context(), targetID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "remove user: preview", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, p)
}

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
	p, err := h.buildRemovalPreview(ctx, targetID)
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
		if req.TransferTo == "" || req.TransferTo == targetID {
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

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.logger.ErrorContext(ctx, "remove user: begin", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	steps := []struct {
		q    string
		args []any
	}{}
	add := func(q string, args ...any) {
		steps = append(steps, struct {
			q    string
			args []any
		}{q, args})
	}
	if req.Mode == "transfer" {
		to := req.TransferTo
		// Host seats on their event types pass to the new owner (dropped where the new
		// owner is already a host there), then ownership moves.
		add(`DELETE FROM event_type_hosts WHERE user_id = ? AND event_type_id IN (
			SELECT event_type_id FROM event_type_hosts WHERE user_id = ?)
			AND event_type_id IN (SELECT id FROM event_types WHERE user_id = ?)`, targetID, to, targetID)
		add(`UPDATE event_type_hosts SET user_id = ? WHERE user_id = ?
			AND event_type_id IN (SELECT id FROM event_types WHERE user_id = ?)`, to, targetID, targetID)
		add(`UPDATE event_types SET user_id = ? WHERE user_id = ?`, to, targetID)
		// Host seats on bookings (past and upcoming) pass to the new member; where they
		// already hold a seat on that booking, the leaver's seat is simply dropped. The
		// admin UI reassigns upcoming primary bookings first through the reassign flow
		// (which moves the calendar event); anything left is moved here.
		add(`DELETE FROM booking_hosts WHERE user_id = ? AND booking_id IN (
			SELECT booking_id FROM booking_hosts WHERE user_id = ?)`, targetID, to)
		add(`UPDATE booking_hosts SET user_id = ? WHERE user_id = ?`, to, targetID)
		add(`UPDATE bookings SET host_id = ? WHERE host_id = ?`, to, targetID)
		add(`UPDATE live_events SET host_user_id = ? WHERE host_user_id = ?`, to, targetID)
	} else {
		// Past history of their event types, and past bookings they hosted elsewhere.
		gone := `SELECT id FROM bookings WHERE event_type_id IN (SELECT id FROM event_types WHERE user_id = ?) OR host_id = ?`
		add(`UPDATE webhook_deliveries SET booking_id = NULL WHERE booking_id IN (`+gone+`)`, targetID, targetID)
		add(`DELETE FROM bookings WHERE id IN (`+gone+`)`, targetID, targetID)
		add(`DELETE FROM event_types WHERE user_id = ?`, targetID)
	}
	add(`UPDATE live_events SET created_by = ? WHERE created_by = ?`, heir, targetID)
	add(`UPDATE team_calendar_shares SET created_by = ? WHERE created_by = ?`, heir, targetID)
	add(`DELETE FROM users WHERE id = ?`, targetID)
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			h.logger.ErrorContext(ctx, "remove user: step", "error", err, "mode", req.Mode)
			h.writeError(w, http.StatusInternalServerError, "could not remove the member: "+err.Error())
			return
		}
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
