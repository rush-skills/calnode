package handler

import (
	"context"

	"github.com/calnode/calnode/internal/webhook"
)

// initiatorFor is the initiated_by value for a change a signed-in user (session or API
// key) makes to a booking: "host" when they host it (as its primary host or on its host
// list), "admin" when they do not but are an admin, and "host" otherwise (a member can
// only change bookings they host, so that is the only way to get here). Call it BEFORE
// the change: a reassignment moves the host it reads.
func (h *Handler) initiatorFor(ctx context.Context, user AuthUser, bookingID string) string {
	var hosts int
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bookings b
		WHERE b.id = ? AND (b.host_id = ? OR EXISTS (
			SELECT 1 FROM booking_hosts bh WHERE bh.booking_id = b.id AND bh.user_id = ?))`,
		bookingID, user.ID, user.ID).Scan(&hosts)
	if hosts == 0 && user.IsAdmin {
		return webhook.InitiatedByAdmin
	}
	return webhook.InitiatedByHost
}

// initiatorFromContext is initiatorFor for the signed-in user in ctx; with no user (a
// path reached without auth) it reports "system".
func (h *Handler) initiatorFromContext(ctx context.Context, bookingID string) string {
	user, ok := userFromContext(ctx)
	if !ok {
		return webhook.InitiatedBySystem
	}
	return h.initiatorFor(ctx, user, bookingID)
}

// mcpInitiator is initiatorFor for an MCP tool call: the MCP caller when there is one,
// else the authenticated user, else "host" (an unscoped connection acts for the owner).
func (h *Handler) mcpInitiator(ctx context.Context, bookingID string) string {
	if c, ok := mcpCallerFromContext(ctx); ok {
		return h.initiatorFor(ctx, AuthUser{ID: c.UserID, IsAdmin: c.IsAdmin}, bookingID)
	}
	if user, ok := userFromContext(ctx); ok {
		return h.initiatorFor(ctx, user, bookingID)
	}
	return webhook.InitiatedByHost
}

// enqueueBookingUpdated sends booking.updated: a stored field a webhook carries changed
// outside create, reschedule, reassign and cancel (today: the calendar sweep minting
// the Meet link or recreating the event). changed names the payload fields that moved.
func (h *Handler) enqueueBookingUpdated(ctx context.Context, bookingID string, changed []string, initiatedBy string) {
	if h.webhookSvc == nil {
		return
	}
	var hostID string
	if err := h.db.QueryRowContext(ctx, `SELECT host_id FROM bookings WHERE id = ?`, bookingID).Scan(&hostID); err != nil {
		h.logger.ErrorContext(ctx, "booking.updated: load booking", "error", err, "booking_id", bookingID)
		return
	}
	// The webhook service fills status, times, location and slug from the stored row.
	if err := h.webhookSvc.Enqueue(ctx, webhook.EventBookingUpdated, webhook.BookingPayload{
		ID: bookingID, HostID: hostID, Changed: changed, InitiatedBy: initiatedBy,
	}); err != nil {
		h.logger.ErrorContext(ctx, "enqueue booking.updated webhook", "error", err, "booking_id", bookingID)
	}
}
