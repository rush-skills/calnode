package handler

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/uid"
)

// ListAPIKeys handles GET /v1/api-keys.
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, name, created_at, last_used_at, scopes
		FROM api_keys WHERE user_id = ?
		ORDER BY created_at DESC`, user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list api keys", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type keyItem struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		CreatedAt  string  `json:"created_at"`
		LastUsedAt *string `json:"last_used_at"`
		// Scopes is null for a full key, else the reads the key is limited to.
		Scopes []string `json:"scopes"`
	}
	items := []keyItem{}
	for rows.Next() {
		var item keyItem
		var scopes sql.NullString
		if err := rows.Scan(&item.ID, &item.Name, &item.CreatedAt, &item.LastUsedAt, &scopes); err != nil {
			h.logger.ErrorContext(r.Context(), "scan api key row", "error", err)
			continue
		}
		if scopes.Valid {
			_ = json.Unmarshal([]byte(scopes.String), &item.Scopes)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list api keys: rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateAPIKey handles POST /v1/api-keys.
// The plaintext key is returned once and must be saved by the caller.
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Name string `json:"name"`
		// Scopes, when given, restrict the key to those reads (apiKeyScopeRoutes).
		// Omitted or null: a full key with the owner's role.
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Name) > 255 {
		h.writeError(w, http.StatusBadRequest, "name must be 255 characters or fewer")
		return
	}
	var scopesArg any // NULL = full key
	if req.Scopes != nil {
		if len(req.Scopes) == 0 {
			h.writeError(w, http.StatusBadRequest, "scopes must list at least one scope, or be omitted for a full key")
			return
		}
		for _, sc := range req.Scopes {
			if !validAPIKeyScope(sc) {
				h.writeError(w, http.StatusBadRequest, "unknown scope: "+sc+" (bookings:read, webhooks:read)")
				return
			}
		}
		b, _ := json.Marshal(req.Scopes)
		scopesArg = string(b)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		h.logger.ErrorContext(r.Context(), "create api key: rand", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	plainKey := "cno_" + hex.EncodeToString(raw)
	keyHash := hashAPIKey(plainKey)

	keyID := uid.New()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO api_keys (id, user_id, name, key_hash, created_at, scopes)
		VALUES (?, ?, ?, ?, ?, ?)`,
		keyID, user.ID, req.Name, keyHash, now, scopesArg); err != nil {
		h.logger.ErrorContext(r.Context(), "create api key: insert", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id":         keyID,
		"name":       req.Name,
		"key":        plainKey,
		"created_at": now,
		"scopes":     req.Scopes,
		"note":       "save this key — it will not be shown again",
	})
}

// DeleteAPIKey handles DELETE /v1/api-keys/{id}.
func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")

	res, err := h.db.ExecContext(r.Context(), `
		DELETE FROM api_keys WHERE id = ? AND user_id = ?`, id, user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "delete api key", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "api key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
