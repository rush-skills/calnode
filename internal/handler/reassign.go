package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/webhook"
)

// ListUserUpcomingBookings handles GET /v1/users/{id}/upcoming-bookings (admin).
// Returns the member's upcoming, non-cancelled bookings as host - every booking where
// they hold a seat (booking_hosts, primary or not) or are the row's host_id - the list
// that drives the "resolve meetings" step before archiving and the removal preview.
func (h *Handler) ListUserUpcomingBookings(w http.ResponseWriter, r *http.Request) {
	actor, ok := userFromContext(r.Context())
	if !ok || !actor.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	hostID := r.PathValue("id")
	now := time.Now().UTC().Format(time.RFC3339Nano)

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT b.id, b.start_at, b.end_at, et.name, et.slug,
		       COALESCE(a.name,''), COALESCE(a.email,'')
		FROM bookings b
		JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN booking_attendees a ON a.booking_id = b.id AND a.is_organizer = 1
		WHERE (b.host_id = ? OR EXISTS (SELECT 1 FROM booking_hosts bh WHERE bh.booking_id = b.id AND bh.user_id = ?))
		  AND b.status != 'cancelled' AND b.end_at > ?
		ORDER BY b.start_at ASC`, hostID, hostID, now)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list upcoming bookings: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type row struct {
		ID            string `json:"id"`
		StartAt       string `json:"start_at"`
		EndAt         string `json:"end_at"`
		EventTypeName string `json:"event_type_name"`
		EventTypeSlug string `json:"event_type_slug"`
		AttendeeName  string `json:"attendee_name"`
		AttendeeEmail string `json:"attendee_email"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ID, &x.StartAt, &x.EndAt, &x.EventTypeName, &x.EventTypeSlug,
			&x.AttendeeName, &x.AttendeeEmail); err != nil {
			continue
		}
		out = append(out, x)
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// errReassignHostUnavailable: the requested new host does not exist or is archived.
var errReassignHostUnavailable = errors.New("reassign: new host not found or archived")

// reassignedBooking is what moving a booking's primary host leaves for the side effects:
// the row as updated plus the old host's calendar event and the summary fields the new
// event and the emails need.
type reassignedBooking struct {
	updated                              *booking.Booking
	oldHostID, newHostID                 string
	extEventID, extProvider              string
	etName, orgName, orgEmail, orgLocale string
	calMsg                               string
}

// reassignBookingRow is the deterministic half of a reassign: the new host is checked
// (active, free at that time), the row and its primary booking_hosts seat move, and what
// the side effects need is captured before the move. Errors are the booking package's
// (ErrDoubleBooked, ErrAlreadyCancelled, ErrNotFound) or errReassignHostUnavailable.
func (h *Handler) reassignBookingRow(ctx context.Context, bookingID, newHostID string) (*reassignedBooking, error) {
	// The new host must exist and be active (not archived).
	var dummy int
	err := h.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ? AND archived_at IS NULL`, newHostID).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errReassignHostUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("reassign: validate host: %w", err)
	}

	// Capture the old host + calendar event + summary fields before the move.
	rb := &reassignedBooking{newHostID: newHostID}
	err = h.db.QueryRowContext(ctx, `
		SELECT b.host_id, COALESCE(b.external_event_id,''), COALESCE(bh.external_provider,''),
		       et.name, COALESCE(a.name,''), COALESCE(a.email,''), COALESCE(a.locale,''),
		       COALESCE(et.calendar_message,'')
		FROM bookings b
		JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN booking_attendees a ON a.booking_id = b.id AND a.is_organizer = 1
		LEFT JOIN booking_hosts bh ON bh.booking_id = b.id AND bh.user_id = b.host_id
		WHERE b.id = ?`, bookingID).
		Scan(&rb.oldHostID, &rb.extEventID, &rb.extProvider, &rb.etName, &rb.orgName, &rb.orgEmail, &rb.orgLocale, &rb.calMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, booking.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reassign: load booking: %w", err)
	}

	rb.updated, err = h.bookingSvc.ReassignHost(ctx, bookingID, newHostID)
	if err != nil {
		return nil, err
	}

	// Keep the multi-host record in sync — the primary host row follows the
	// booking's host_id so later cancel/notify fan-out targets the right person.
	// (Reassign is single-host; the primary row is the one being moved.)
	if _, err := h.db.ExecContext(ctx,
		`UPDATE booking_hosts SET user_id = ?, external_event_id = NULL WHERE booking_id = ? AND is_primary = 1`,
		newHostID, rb.updated.ID); err != nil {
		h.logger.Error("reassign: sync booking_hosts", "error", err, "booking_id", bookingID)
	}
	return rb, nil
}

// reassignSideEffects moves the calendar event (cancelled on the old host's calendar,
// recreated on the new host's) and notifies the attendee and the new host. It waits for
// the provider calls: the HTTP handler runs it in a goroutine, member removal inline
// before the old host's row is deleted (a cancel as a deleted user cannot route).
func (h *Handler) reassignSideEffects(ctx context.Context, rb *reassignedBooking) {
	bCopy := *rb.updated
	newHostID := rb.newHostID
	inviteMode := bCopy.InviteDelivery

	// Move the Google Calendar event: remove from the old host, recreate on
	// the new host, and persist the new event ID (clearing it if recreation
	// produced nothing, e.g. the new host has no destination calendar).
	if gc := h.getCal(); gc != nil {
		if rb.extEventID != "" {
			// Reassignment cancels on the OLD host's calendar. Their stamped provider
			// routes it ("" falls back to id recognition, then their destination).
			h.dropCalendarManageLinks(ctx, gc, rb.oldHostID, "", rb.extEventID, rb.extProvider, bCopy.ID)
			if err := gc.CancelEvent(ctx, rb.oldHostID, "", rb.extEventID, rb.extProvider); err != nil {
				h.logger.Error("reassign: delete old calendar event", "error", err, "booking_id", bCopy.ID)
			}
		}
		loc := i18n.Get(rb.orgLocale) // nil (→ English) if empty/unrecognized; i18n.Locale.T handles nil safely
		// The recreated event carries the default participants like the original did,
		// minus the booker and the booking's hosts (booking_hosts already names the new
		// host - it was synced by reassignBookingRow).
		var extra []string
		if defaults := h.loadDefaultAttendees(ctx, "reassign"); len(defaults) > 0 {
			extra = extraAttendeesFor(defaults, append([]string{rb.orgEmail}, h.bookingHostEmails(ctx, bCopy.ID)...)...)
		}
		answers, aerr := h.loadAnswerLines(ctx, bCopy.ID)
		if aerr != nil {
			h.logger.Error("reassign: load answers for calendar event", "error", aerr, "booking_id", bCopy.ID)
		}
		descPlain, descRich := calendarDescription(loc, h.withOrgCalendarMessage(ctx, rb.calMsg), answers, h.calendarManageLinks(ctx, bCopy.ID), bCopy.ID)
		newEventID, _, newCalID, newProvider, err := gc.CreateEvent(ctx, newHostID, calendar.CreateEventParams{
			Summary:         loc.Tf("calendar_event_summary", rb.etName, rb.orgName),
			Description:     descPlain,
			DescriptionHTML: descRich,
			Location:        bCopy.LocationValue, // keep the existing Meet link (don't mint a new one)
			Start:           bCopy.StartAt,
			End:             bCopy.EndAt,
			OrganizerName:   rb.orgName,
			OrganizerEmail:  calendarInvitee(inviteMode, rb.orgEmail),
			ExtraAttendees:  extra,
		})
		if err != nil {
			h.logger.Error("reassign: create new calendar event", "error", err, "booking_id", bCopy.ID)
		} else {
			if _, err := h.db.ExecContext(ctx,
				`UPDATE bookings SET external_event_id = ? WHERE id = ?`, newEventID, bCopy.ID); err != nil {
				h.logger.Error("reassign: persist new event id", "error", err, "booking_id", bCopy.ID)
			}
			if _, err := h.db.ExecContext(ctx,
				`UPDATE booking_hosts SET external_event_id = ?, external_calendar_id = ?,
				 external_provider = ?
				 WHERE booking_id = ? AND is_primary = 1`,
				newEventID, newCalID, newProvider, bCopy.ID); err != nil {
				h.logger.Error("reassign: persist host event id", "error", err, "booking_id", bCopy.ID)
			}
		}
	}

	// Notify the attendee (their host changed) and the new host, reusing the
	// confirmation templates. Host details are overridden to the new host.
	d, err := h.loadCancellationData(ctx, &bCopy)
	if err != nil {
		h.logger.Error("reassign: load email data", "error", err, "booking_id", bCopy.ID)
		return
	}
	d.BaseURL = h.publicURL()
	h.applyBranding(ctx, &d)
	if err := h.loadHostIntoData(ctx, newHostID, &d); err != nil {
		h.logger.Error("reassign: load new host", "error", err, "booking_id", bCopy.ID)
	}

	// A Calnode-sent invite is re-issued (same UID, newer SEQUENCE) so the booker's
	// calendar entry follows the change. A calendar-sent one needs nothing here: the
	// new host's calendar invited the booker when the event was recreated above.
	if inviteMode == booking.InviteByCalnode {
		h.applyInviteDelivery(ctx, &d, inviteMode, newHostID)
		d.ICSSequence = int(time.Now().Unix())
	}
	prefs := h.hostPrefsOrDefault(ctx, bCopy.ID, newHostID)
	if prefs.NotifyConfirmation {
		if err := mailer.SendConfirmationToAttendee(ctx, h.mailer, d); err != nil {
			h.logger.Error("reassign: email attendee", "error", err, "booking_id", bCopy.ID)
		}
	}
	if prefs.NotifyHostBooking {
		hd := d
		h.applyHostInvite(ctx, &hd, inviteMode, newHostID)
		hd.ICSSequence = int(time.Now().Unix())
		if err := mailer.SendConfirmationToHost(ctx, h.mailer, hd); err != nil {
			h.logger.Error("reassign: email new host", "error", err, "booking_id", bCopy.ID)
		}
	}

	if h.webhookSvc != nil {
		if err := h.webhookSvc.Enqueue(ctx, "booking.rescheduled", webhook.BookingPayload{
			ID:            bCopy.ID,
			EventTypeSlug: d.EventTypeSlug,
			HostID:        newHostID,
			StartAt:       bCopy.StartAt.UTC().Format(time.RFC3339),
			EndAt:         bCopy.EndAt.UTC().Format(time.RFC3339),
			Status:        bCopy.Status,
			LocationValue: bCopy.LocationValue,
			CreatedAt:     bCopy.CreatedAt.UTC().Format(time.RFC3339),
			InitiatedBy:   webhook.InitiatedByHost,
		}); err != nil {
			h.logger.Error("reassign: enqueue webhook", "error", err, "booking_id", bCopy.ID)
		}
	}
}

// ReassignBooking handles POST /v1/bookings/{id}/reassign (admin).
// Body: {"host_id":"<userId>"}. Moves the booking to another active host,
// checking the new host is free at that time, then (async) moves the Google
// Calendar event and notifies the attendee and the new host.
func (h *Handler) ReassignBooking(w http.ResponseWriter, r *http.Request) {
	actor, ok := userFromContext(r.Context())
	if !ok || !actor.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		HostID string `json:"host_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostID == "" {
		h.writeError(w, http.StatusBadRequest, "host_id is required")
		return
	}

	rb, err := h.reassignBookingRow(r.Context(), id, req.HostID)
	switch {
	case errors.Is(err, errReassignHostUnavailable):
		h.writeError(w, http.StatusBadRequest, "new host not found or archived")
		return
	case errors.Is(err, booking.ErrDoubleBooked):
		h.writeError(w, http.StatusConflict, "the chosen host already has a booking at that time")
		return
	case errors.Is(err, booking.ErrAlreadyCancelled):
		h.writeError(w, http.StatusConflict, "this booking has been cancelled")
		return
	case errors.Is(err, booking.ErrNotFound):
		h.writeError(w, http.StatusNotFound, "booking not found")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "reassign: update", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, toBookingJSON(rb.updated))

	// Side effects: move the calendar event and notify attendee + new host.
	go func() { // #nosec G118 -- deliberately its own context.Background(); the request context is cancelled the moment this handler returns
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.reassignSideEffects(ctx, rb)
	}()
}
