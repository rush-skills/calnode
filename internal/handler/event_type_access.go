package handler

import (
	"database/sql"
	"errors"
	"net/http"
)

// Fork: event type visibility (event_types.visibility, fork migration 00006).
//
// Upstream decides who may CHANGE an event type (canManageEventType: its owner or any
// admin). The fork adds who may SEE one:
//
//   - "org" (default): every signed-in member sees it in the admin UI, read-only unless
//     canManageEventType lets them change it.
//   - "private": the owner, its assigned hosts and admins, as upstream has it.
//
// Only the owner changes visibility (PatchEventType). Unrelated to is_public, which
// governs the public directory pages.
const (
	eventTypeVisibilityOrg     = "org"
	eventTypeVisibilityPrivate = "private"
)

// validEventTypeVisibility reports whether v is a value the column accepts.
func validEventTypeVisibility(v string) bool {
	return v == eventTypeVisibilityOrg || v == eventTypeVisibilityPrivate
}

// eventTypeIDForViewer resolves an event type the caller may READ: one they may manage,
// one they host, or an org-wide one. The admin-side reads behind the editor (hosts,
// questions) use it, so a member opening a shared event type read-only sees what an
// editor sees. 404 when the slug is unknown or the event type is private to someone
// else.
func (h *Handler) eventTypeIDForViewer(w http.ResponseWriter, r *http.Request, slug string, user AuthUser) string {
	var id string
	err := h.db.QueryRowContext(r.Context(), `
		SELECT id FROM event_types
		WHERE slug = ? AND (? = 1 OR visibility = 'org' OR user_id = ?
			OR id IN (SELECT event_type_id FROM event_type_hosts WHERE user_id = ?))`,
		slug, boolArg(user.IsAdmin), user.ID, user.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return ""
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "event type: resolve for viewer", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return ""
	}
	return id
}
