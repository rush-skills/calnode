package handler

import (
	"database/sql"
	"errors"
	"net/http"
)

// Event type visibility values (event_types.visibility).
//
//   - "org": every signed-in member sees it in the admin UI; the owner and any admin
//     may edit it.
//   - "private": only the owner sees and edits it (assigned hosts still see it
//     read-only, as they always have — they need to know what they are hosting).
//
// Unrelated to is_public, which governs the public directory pages.
const (
	eventTypeVisibilityOrg     = "org"
	eventTypeVisibilityPrivate = "private"
)

// validEventTypeVisibility reports whether v is a value the column accepts.
func validEventTypeVisibility(v string) bool {
	return v == eventTypeVisibilityOrg || v == eventTypeVisibilityPrivate
}

// canEditEventType is THE rule for who may change an event type: its owner, or any
// admin when it is organisation-wide. Private event types stay owner-only, so an admin
// cannot reach into something a member deliberately kept to themselves.
//
// Every write handler goes through eventTypeIDForEditor, which applies this; nothing
// else should re-derive it from user_id.
func canEditEventType(user AuthUser, ownerID, visibility string) bool {
	return user.ID == ownerID || (user.IsAdmin && visibility == eventTypeVisibilityOrg)
}

// eventTypeVisibleFilter is the SQL predicate for "this member may see the row" —
// org-wide, their own, or one they are assigned to host. It expects two bound
// arguments, both the viewer's user id, and references event_types unqualified so it
// composes with the list/get queries in event_type.go.
const eventTypeVisibleFilter = `(visibility = 'org'
	OR user_id = ?
	OR id IN (SELECT event_type_id FROM event_type_hosts WHERE user_id = ?))`

// eventTypeRef is the little an access check needs to know about an event type.
type eventTypeRef struct {
	ID         string
	OwnerID    string
	Visibility string
}

// lookupEventTypeRef resolves a slug; (nil, nil) when it does not exist.
func (h *Handler) lookupEventTypeRef(r *http.Request, slug string) (*eventTypeRef, error) {
	var ref eventTypeRef
	err := h.db.QueryRowContext(r.Context(),
		`SELECT id, user_id, visibility FROM event_types WHERE slug = ?`, slug).
		Scan(&ref.ID, &ref.OwnerID, &ref.Visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// eventTypeForEditor resolves an event type the caller may WRITE to. It writes the
// response and returns nil when it cannot: 404 for an unknown slug, 403 for one the
// caller can see but not change (a member on an org-wide event type they do not own,
// or an admin on someone else's private one).
//
// 403 rather than 404 for the "exists but not yours" case is deliberate: the slug is
// the public booking URL, so its existence is no secret, and a member who just saw the
// event type in their list deserves to be told why the save failed.
func (h *Handler) eventTypeForEditor(w http.ResponseWriter, r *http.Request, slug string, user AuthUser) *eventTypeRef {
	ref, err := h.lookupEventTypeRef(r, slug)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "event type: resolve for editor", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	if ref == nil {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return nil
	}
	if !canEditEventType(user, ref.OwnerID, ref.Visibility) {
		h.writeError(w, http.StatusForbidden,
			"only the owner of this event type (or an admin, for organisation-wide ones) can change it")
		return nil
	}
	return ref
}

// eventTypeIDForEditor is eventTypeForEditor for callers that only need the id. It
// replaces every former "WHERE slug = ? AND user_id = ?" write guard; "" means the
// response has already been written.
func (h *Handler) eventTypeIDForEditor(w http.ResponseWriter, r *http.Request, slug string, user AuthUser) string {
	if ref := h.eventTypeForEditor(w, r, slug, user); ref != nil {
		return ref.ID
	}
	return ""
}

// eventTypeIDForViewer resolves an event type the caller may READ: org-wide, their
// own, or one they host. Used by the admin-side reads that back the editor (hosts,
// questions) so a read-only viewer sees the same thing an editor does. 404 when the
// slug is unknown OR the event type is private to someone else — a private event type
// is simply not there, as far as the rest of the workspace is concerned.
func (h *Handler) eventTypeIDForViewer(w http.ResponseWriter, r *http.Request, slug string, user AuthUser) string {
	var id string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT id FROM event_types WHERE slug = ? AND `+eventTypeVisibleFilter,
		slug, user.ID, user.ID).Scan(&id)
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
