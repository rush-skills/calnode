package handler

import (
	"database/sql"
	"net/http"
)

// ListUsers handles GET /v1/users — admin only. Returns all users.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	// Any signed-in member may read the directory: the event-type Hosts tab and the
	// team picker need it, and a member setting up a rotation cannot pick colleagues
	// they cannot see. Mutations elsewhere stay admin-only.
	user, ok := userFromContext(r.Context())
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Archived members are hidden unless explicitly requested (?include_archived=true),
	// and only an admin gets to ask: the Members page's archived view is admin-only.
	includeArchived := user.IsAdmin && r.URL.Query().Get("include_archived") == "true"
	// The "Former member" tombstone (user_removal.go) is not a member: it is never listed,
	// archived view or not.
	where := "WHERE u.archived_at IS NULL AND NOT " + formerMemberSQL
	if includeArchived {
		where = "WHERE NOT " + formerMemberSQL
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.name, u.iana_timezone, u.is_admin, u.is_owner, u.email_login,
		       COALESCE(u.provider,''), COALESCE(u.avatar_url,''), COALESCE(u.handle,''), u.created_at,
		       u.archived_at, COALESCE(u.archived_by,''), COALESCE(ab.name,''),
		       EXISTS (SELECT 1 FROM calendar_connections cc WHERE cc.user_id = u.id)
		FROM users u LEFT JOIN users ab ON ab.id = u.archived_by
		`+where+` ORDER BY u.created_at ASC`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list users: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type teamRef struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type userRow struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		Name           string `json:"name"`
		Handle         string `json:"handle,omitempty"`
		Timezone       string `json:"timezone"`
		IsAdmin        bool   `json:"is_admin"`
		IsOwner        bool   `json:"is_owner"`
		Role           string `json:"role"` // "owner" | "admin" | "member"
		EmailLogin     bool   `json:"email_login"`
		Provider       string `json:"provider,omitempty"`
		AvatarURL      string `json:"avatar_url,omitempty"`
		CreatedAt      string `json:"created_at"`
		Archived       bool   `json:"archived"`
		ArchivedAt     string `json:"archived_at,omitempty"`
		ArchivedBy     string `json:"archived_by,omitempty"`
		ArchivedByName string `json:"archived_by_name,omitempty"`
		// HasCalendar is false when the member has no connected calendar at all. A
		// host in that state sends no invites for their bookings, silently: the
		// editor's Hosts tab and the members page warn on it.
		HasCalendar bool      `json:"has_calendar"`
		Teams       []teamRef `json:"teams"`
	}
	out := []userRow{}
	byID := map[string]*userRow{}
	for rows.Next() {
		var u userRow
		var isAdmin, isOwner, emailLogin, hasCal int
		var archivedAt sql.NullString
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Timezone, &isAdmin, &isOwner, &emailLogin,
			&u.Provider, &u.AvatarURL, &u.Handle, &u.CreatedAt, &archivedAt, &u.ArchivedBy, &u.ArchivedByName, &hasCal); err != nil {
			continue
		}
		u.HasCalendar = hasCal != 0
		u.IsAdmin = isAdmin != 0
		u.IsOwner = isOwner != 0
		u.EmailLogin = emailLogin != 0
		u.Archived = archivedAt.Valid
		u.ArchivedAt = archivedAt.String
		u.Teams = []teamRef{}
		switch {
		case u.IsOwner:
			u.Role = "owner"
		case u.IsAdmin:
			u.Role = "admin"
		default:
			u.Role = "member"
		}
		out = append(out, u)
	}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	// Close before the next query: the pool is a single connection, and an open
	// cursor holds it (exhausted is not closed) — querying while rows is open
	// deadlocks.
	rows.Close() // #nosec G104 -- rows already fully consumed above; nothing actionable on close error
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list users: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Attach each member's teams (the Members↔Teams cross-reference).
	tmRows, err := h.db.QueryContext(r.Context(), `
		SELECT tm.user_id, t.id, t.name
		FROM team_members tm JOIN teams t ON t.id = tm.team_id
		ORDER BY t.name ASC`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list users: team memberships", "error", err)
	} else {
		defer tmRows.Close()
		for tmRows.Next() {
			var userID string
			var tr teamRef
			if err := tmRows.Scan(&userID, &tr.ID, &tr.Name); err != nil {
				continue
			}
			if u := byID[userID]; u != nil {
				u.Teams = append(u.Teams, tr)
			}
		}
	}

	h.writeJSON(w, http.StatusOK, out)
}
