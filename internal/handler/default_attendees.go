package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/calnode/calnode/internal/richtext"
	"net/http"
	"net/mail"
	"strings"
)

// maxDefaultAttendees bounds the workspace's default participant list. It is a guard against
// an operator pasting a mailing list rather than a product limit: every address lands on every
// host calendar event created on the platform.
const maxDefaultAttendees = 20

// defaultAttendeeEmails returns the workspace's default participants - the addresses invited
// on the host's calendar event of every booking (see docs/features/default-participants.md).
// The column is comma-separated and normalised on write; reading lower-cases again anyway so
// the invariant every consumer relies on holds for a row written by a seeder or by hand. A
// missing settings row (a database older than migration 00011, or a test that never ran
// setup) is "no defaults", not an error: nil, nil.
func (h *Handler) defaultAttendeeEmails(ctx context.Context) ([]string, error) {
	var raw string
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(default_attendee_emails, '') FROM server_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("default attendees: load: %w", err)
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// bookingHostEmails returns the addresses of every host on a booking, so the reconcile and
// reassign paths exclude the same people from the default participants as the inline create
// does (which has the hosts in memory). On a group booking a host who is also a default
// participant must not be invited to a co-host's event either. A query failure is logged and
// yields nil: the worst case is a duplicate invite, never a blocked event.
func (h *Handler) bookingHostEmails(ctx context.Context, bookingID string) []string {
	rows, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(u.email, '') FROM booking_hosts bh
		JOIN users u ON u.id = bh.user_id
		WHERE bh.booking_id = ?`, bookingID)
	if err != nil {
		h.logger.Error("default participants: load booking hosts", "error", err, "booking_id", bookingID)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err == nil && e != "" {
			out = append(out, e)
		}
	}
	return out
}

// normalizeAttendeeEmails trims, lower-cases and dedupes a list of addresses, validating each
// with net/mail.ParseAddress. Only a bare address is accepted - "Bot <bot@example.com>" is
// refused rather than unwrapped, because the stored form is comma-separated and a display name
// could carry a comma. The error names the offending entry so the admin UI can show it.
// Blank entries are skipped (a trailing comma or newline in a textarea is not a mistake).
func normalizeAttendeeEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		e := strings.ToLower(strings.TrimSpace(raw))
		if e == "" {
			continue
		}
		addr, err := mail.ParseAddress(e)
		if err != nil || addr.Name != "" || addr.Address != e || strings.ContainsAny(e, ",;<>\" \t\r\n") {
			return nil, fmt.Errorf("%q is not a valid email address", strings.TrimSpace(raw))
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) > maxDefaultAttendees {
		return nil, fmt.Errorf("at most %d default participants are allowed (got %d)", maxDefaultAttendees, len(out))
	}
	return out, nil
}

// extraAttendeesFor filters the default participants down to the ones that should actually be
// added to one host's calendar event: anyone who is already on it - the organizer (booker) or
// a host - is left out, so a shared mailbox that is also a host is never invited twice.
// Comparison is case-insensitive; the defaults are stored lower-case but booker and host
// addresses are stored as typed. Returns nil when nothing remains, so a provider request
// without defaults is byte-identical to one before this feature existed.
func extraAttendeesFor(defaults []string, exclude ...string) []string {
	if len(defaults) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			skip[e] = true
		}
	}
	var out []string
	for _, d := range defaults {
		if !skip[strings.ToLower(d)] {
			out = append(out, d)
		}
	}
	return out
}

// loadDefaultAttendees is defaultAttendeeEmails for the booking paths: a load failure is logged
// and treated as "no defaults", because a default participant must never block a booking or a
// reconcile sweep. Callers load once per operation, not once per host.
func (h *Handler) loadDefaultAttendees(ctx context.Context, where string) []string {
	defaults, err := h.defaultAttendeeEmails(ctx)
	if err != nil {
		h.logger.Error(where+": load default participants; proceeding without them", "error", err)
		return nil
	}
	return defaults
}

// GetParticipantSettings handles GET /v1/settings/participants (admin).
func (h *Handler) GetParticipantSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	emails, err := h.defaultAttendeeEmails(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "participant settings: load", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if emails == nil {
		emails = []string{}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"default_attendee_emails":  emails,
		"default_calendar_message": h.orgCalendarMessage(r.Context()),
	})
}

// maxOrgCalendarMessage bounds the stored HTML; calendar providers cap descriptions too.
const maxOrgCalendarMessage = 8000

// orgCalendarMessage returns the workspace's default invite message (sanitized HTML), or
// "" when unset or unreadable. A read failure is logged and treated as "no message" so it
// can never block a booking.
func (h *Handler) orgCalendarMessage(ctx context.Context) string {
	var raw string
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(default_calendar_message, '') FROM server_settings WHERE id = 1`).Scan(&raw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.logger.ErrorContext(ctx, "load default calendar message", "error", err)
		}
		return ""
	}
	return richtext.Sanitize(raw)
}

// withOrgCalendarMessage appends the workspace's default invite message after an event
// type's own calendar message. It is applied wherever a booking's invite text is built
// (new bookings, reconcile, reassign, reschedule/cancel emails), and read at that moment,
// so a change reaches every event type from the next booking on.
func (h *Handler) withOrgCalendarMessage(ctx context.Context, eventTypeMessageHTML string) string {
	org := h.orgCalendarMessage(ctx)
	et := richtext.Sanitize(eventTypeMessageHTML)
	switch {
	case org == "":
		return et
	case et == "":
		return org
	default:
		return et + org
	}
}

// PatchParticipantSettings handles PATCH /v1/settings/participants (admin). The list is a
// pointer so "omitted" keeps the stored value while an explicit [] clears it.
func (h *Handler) PatchParticipantSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		DefaultAttendeeEmails *[]string `json:"default_attendee_emails"`
		// Pointer: omitted keeps, "" clears.
		DefaultCalendarMessage *string `json:"default_calendar_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.DefaultAttendeeEmails != nil {
		emails, err := normalizeAttendeeEmails(*req.DefaultAttendeeEmails)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.db.ExecContext(r.Context(),
			`UPDATE server_settings SET default_attendee_emails = ?, updated_at = datetime('now') WHERE id = 1`,
			strings.Join(emails, ",")); err != nil {
			h.logger.ErrorContext(r.Context(), "participant settings: update", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if req.DefaultCalendarMessage != nil {
		msg := richtext.Sanitize(*req.DefaultCalendarMessage)
		if len(msg) > maxOrgCalendarMessage {
			h.writeError(w, http.StatusBadRequest, "the default invite message is too long")
			return
		}
		if _, err := h.db.ExecContext(r.Context(),
			`UPDATE server_settings SET default_calendar_message = ?, updated_at = datetime('now') WHERE id = 1`,
			msg); err != nil {
			h.logger.ErrorContext(r.Context(), "participant settings: update message", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	h.GetParticipantSettings(w, r)
}
