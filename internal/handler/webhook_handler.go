package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/calnode/calnode/internal/netutil"
	"github.com/calnode/calnode/internal/webhook"
)

var validWebhookEvents = []string{
	"booking.created", "booking.cancelled", "booking.rescheduled", "booking.reassigned",
	"booking.updated", "booking.rsvp",
	"recording.completed", "transcript.ready", "notes.ready",
}

func (h *Handler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Fields []string `json:"fields"` // optional payload field selection; nil = default set
		// Scope is "user" (default: bookings the creator hosts) or "org" (every booking in
		// the workspace). Only an admin may create an org webhook.
		Scope string `json:"scope"`
		// EventTypes limits the webhook to bookings of these event type slugs; empty or
		// omitted means every event type.
		EventTypes []string `json:"event_types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL == "" {
		h.writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	u, err := url.ParseRequestURI(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		h.writeError(w, http.StatusBadRequest, "url must be a valid http or https URL")
		return
	}
	if err := validateWebhookURL(r.Context(), u); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Events) == 0 {
		h.writeError(w, http.StatusBadRequest, "events must not be empty")
		return
	}
	switch req.Scope {
	case "", webhook.ScopeUser:
		req.Scope = webhook.ScopeUser
	case webhook.ScopeOrg:
		if !user.IsAdmin {
			h.writeError(w, http.StatusForbidden, "only an admin can create an organisation-wide webhook")
			return
		}
	default:
		h.writeError(w, http.StatusBadRequest, "scope must be 'user' or 'org'")
		return
	}
	for _, e := range req.Events {
		if !slices.Contains(validWebhookEvents, e) {
			h.writeError(w, http.StatusBadRequest, "unknown event: "+e)
			return
		}
	}

	wh, secret, err := h.webhookSvc.Create(r.Context(), user.ID, req.URL, req.Events)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Apply the field selection (if any) as a follow-up update so Create keeps its
	// stable signature; unknown keys are filtered out.
	if req.Fields != nil {
		if err := h.webhookSvc.Update(r.Context(), user.ID, wh.ID, nil, &req.Fields); err != nil {
			h.logger.ErrorContext(r.Context(), "create webhook: set fields", "error", err)
		} else {
			wh.Fields = webhook.ValidFields(req.Fields)
		}
	}
	if len(req.EventTypes) > 0 {
		if err := h.webhookSvc.SetEventTypes(r.Context(), user.ID, wh.ID, req.EventTypes); err != nil {
			_ = h.webhookSvc.Delete(r.Context(), user.ID, wh.ID)
			h.logger.ErrorContext(r.Context(), "create webhook: set event types", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		wh.EventTypes = req.EventTypes
	}
	if req.Scope == webhook.ScopeOrg {
		if err := h.webhookSvc.SetScope(r.Context(), user.ID, wh.ID, webhook.ScopeOrg); err != nil {
			// Never leave behind a user-scoped hook the admin did not ask for.
			_ = h.webhookSvc.Delete(r.Context(), user.ID, wh.ID)
			h.logger.ErrorContext(r.Context(), "create webhook: set scope", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		wh.Scope = webhook.ScopeOrg
	}

	h.writeJSON(w, http.StatusCreated, map[string]any{
		"id":          wh.ID,
		"url":         wh.URL,
		"events":      wh.Events,
		"fields":      wh.Fields,
		"scope":       wh.Scope,
		"event_types": nonNilStrings(wh.EventTypes),
		"secret":      secret,
		"secret_note": webhookSecretNote,
		"is_active":   wh.IsActive,
		"created_at":  wh.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// PatchWebhook handles PATCH /v1/webhooks/{id} — update events and/or the payload
// field selection of an existing webhook.
func (h *Handler) PatchWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		Events     *[]string `json:"events"`
		Fields     *[]string `json:"fields"`
		EventTypes *[]string `json:"event_types"` // [] clears the filter
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Events != nil {
		if len(*req.Events) == 0 {
			h.writeError(w, http.StatusBadRequest, "events must not be empty")
			return
		}
		for _, e := range *req.Events {
			if !slices.Contains(validWebhookEvents, e) {
				h.writeError(w, http.StatusBadRequest, "unknown event: "+e)
				return
			}
		}
	}
	owner := h.webhookOwner(r.Context(), user, id)
	if req.EventTypes != nil {
		if err := h.webhookSvc.SetEventTypes(r.Context(), owner, id, *req.EventTypes); err != nil {
			if errors.Is(err, webhook.ErrNotFound) {
				h.writeError(w, http.StatusNotFound, "webhook not found")
				return
			}
			h.logger.ErrorContext(r.Context(), "update webhook: event types", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := h.webhookSvc.Update(r.Context(), owner, id, req.Events, req.Fields); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "update webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	webhooks, err := h.webhookSvc.List(r.Context(), user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list webhooks", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Admins also see, and manage, the organisation-wide webhooks other admins made.
	if user.IsAdmin {
		org, err := h.webhookSvc.ListOrg(r.Context())
		if err != nil {
			h.logger.ErrorContext(r.Context(), "list org webhooks", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, wh := range org {
			if wh.UserID != user.ID {
				webhooks = append(webhooks, wh)
			}
		}
	}

	items := make([]map[string]any, len(webhooks))
	for i, wh := range webhooks {
		prev := ""
		if wh.PreviousSecretUntil != nil {
			prev = wh.PreviousSecretUntil.UTC().Format(time.RFC3339)
		}
		items[i] = map[string]any{
			"id":          wh.ID,
			"url":         wh.URL,
			"events":      wh.Events,
			"fields":      wh.Fields,
			"scope":       wh.Scope,
			"event_types": nonNilStrings(wh.EventTypes),
			"is_active":   wh.IsActive,
			// Set while a rotated-out secret still signs deliveries alongside the new one.
			"previous_secret_valid_until": prev,
			"created_at":                  wh.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")

	if err := h.webhookSvc.Delete(r.Context(), h.webhookOwner(r.Context(), user, id), id); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "delete webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	webhookID := r.PathValue("id")

	deliveries, err := h.webhookSvc.ListDeliveries(r.Context(), h.webhookOwner(r.Context(), user, webhookID), webhookID)
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "list deliveries", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	items := make([]map[string]any, len(deliveries))
	for i, d := range deliveries {
		item := map[string]any{
			"id":            d.ID,
			"webhook_id":    d.WebhookID,
			"event":         d.Event,
			"status":        d.Status,
			"attempt_count": d.AttemptCount,
		}
		if d.BookingID != "" {
			item["booking_id"] = d.BookingID
		}
		if d.ResponseStatus != nil {
			item["response_status"] = *d.ResponseStatus
		}
		if d.LastAttemptedAt != nil {
			item["last_attempted_at"] = *d.LastAttemptedAt
		}
		items[i] = item
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// validateWebhookURL resolves the URL host and rejects any address in a
// loopback, link-local, or private range to prevent SSRF.
// validateWebhookURL rejects a webhook URL whose host resolves (now) to a private/
// loopback address — the same SSRF check the worker re-applies at actual delivery
// time (netutil.ResolveSafe), since DNS can change between saving a URL and
// delivering to it.
func validateWebhookURL(ctx context.Context, u *url.URL) error {
	if _, err := netutil.ResolveSafe(ctx, u.Hostname()); err != nil {
		return fmt.Errorf("webhook URL must not resolve to a private or loopback address: %w", err)
	}
	return nil
}

// webhookOwner is the user ID to act on webhook id as. Normally the caller: the service
// only touches a webhook its owner names. An admin acting on an organisation-wide
// webhook acts as that webhook's creator, so any admin can manage any org webhook.
func (h *Handler) webhookOwner(ctx context.Context, user AuthUser, id string) string {
	if user.IsAdmin {
		if owner, ok := h.webhookSvc.OrgOwner(ctx, id); ok {
			return owner
		}
	}
	return user.ID
}

// webhookSecretNote tells an integrator how to use the secret, because the obvious
// reading (HMAC keyed by the hex string) is wrong and fails every verification.
const webhookSecretNote = "Shown once. Verify X-Calnode-Signature as HMAC-SHA256 of the raw request body, " +
	"keyed by the hex-DECODED secret (32 bytes), not the hex string. See docs/webhooks.md."

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// RotateWebhookSecret handles POST /v1/webhooks/{id}/rotate-secret: a new signing secret,
// returned once. The old one keeps signing alongside it for 24 hours, so deliveries in
// that window carry both signatures and the receiver can switch without dropping any.
func (h *Handler) RotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id := r.PathValue("id")
	secret, until, err := h.webhookSvc.RotateSecret(r.Context(), h.webhookOwner(r.Context(), user, id), id)
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "rotate webhook secret", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"id":                          id,
		"secret":                      secret,
		"secret_note":                 webhookSecretNote,
		"previous_secret_valid_until": until.Format(time.RFC3339),
	})
}

// RedeliverWebhookDelivery handles POST /v1/webhooks/{id}/deliveries/{delivery_id}/redeliver:
// send a delivery again now, with a fresh set of attempts. Same delivery id and payload.
func (h *Handler) RedeliverWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	id, deliveryID := r.PathValue("id"), r.PathValue("delivery_id")
	if err := h.webhookSvc.Redeliver(r.Context(), h.webhookOwner(r.Context(), user, id), id, deliveryID); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "delivery not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "redeliver webhook", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
