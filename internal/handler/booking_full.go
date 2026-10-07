package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/bookingsnap"
)

// bookingIncludes is which full-shape parts a booking read asked for.
type bookingIncludes struct{ hosts, attendees, answers, meeting bool }

func (i bookingIncludes) any() bool { return i.hosts || i.attendees || i.answers || i.meeting }

var allIncludes = bookingIncludes{true, true, true, true}

// parseIncludes reads include=hosts,attendees,answers,meeting.
func parseIncludes(v string) (bookingIncludes, error) {
	var inc bookingIncludes
	for _, p := range strings.Split(v, ",") {
		switch strings.TrimSpace(p) {
		case "":
		case "hosts":
			inc.hosts = true
		case "attendees":
			inc.attendees = true
		case "answers":
			inc.answers = true
		case "meeting":
			inc.meeting = true
		default:
			return inc, fmt.Errorf("include takes hosts, attendees, answers, meeting")
		}
	}
	return inc, nil
}

// applySnapshot fills j's full-shape fields from the booking as stored now, the same
// data a webhook payload carries (bookingsnap), so a receiver repairing its copy from
// the API sees what the webhooks sent.
func (h *Handler) applySnapshot(ctx context.Context, j *bookingJSON, inc bookingIncludes) error {
	s, err := bookingsnap.Load(ctx, h.db, j.ID, h.baseURL)
	if err != nil {
		return err
	}
	j.EventTypeSlug, j.EventTypeName = s.EventTypeSlug, s.EventTypeName
	j.Revision, j.ChangedAt, j.AdminURL = s.Revision, s.ChangedAt, s.AdminURL
	if inc.meeting {
		m := s.Meeting
		j.Meeting = &m
	}
	if inc.answers {
		j.Answers = s.Answers
	}
	if inc.hosts {
		j.Hosts = make([]hostBrief, 0, len(s.Hosts))
		for _, x := range s.Hosts {
			j.Hosts = append(j.Hosts, hostBrief{ID: x.ID, Name: x.Name, Email: x.Email, Role: x.Role})
		}
	}
	if inc.attendees {
		j.Attendees = make([]attendeeJSON, 0, len(s.Attendees))
		for _, a := range s.Attendees {
			org := a.Organizer
			j.Attendees = append(j.Attendees, attendeeJSON{Name: a.Name, Email: a.Email, Timezone: a.Timezone,
				Phone: a.Phone, Locale: a.Locale, RSVPStatus: a.RSVPStatus, Organizer: &org})
		}
	}
	return nil
}

// GetBooking handles GET /v1/bookings/{id}. It used to need no credentials, and the
// booking id is printed on every calendar invite, so anyone the invite reached could
// read the join link and the cancellation reason. It now takes either the booking's
// manage token (?token=, what the booker holds) or a signed-in user / API key that may
// see the booking (an admin, or one of its hosts). Either way the response is the full
// shape a webhook carries.
func (h *Handler) GetBooking(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if tok := r.URL.Query().Get("token"); tok != "" {
		b, err := h.bookingSvc.ValidateManageToken(r.Context(), tok)
		if err != nil || b.ID != id {
			h.writeError(w, http.StatusNotFound, "booking not found")
			return
		}
		h.writeFullBooking(w, r, b)
		return
	}
	h.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		user, _ := userFromContext(r.Context())
		b, err := h.bookingSvc.Get(r.Context(), id)
		if err != nil || (!user.IsAdmin && !h.userHostsBooking(r.Context(), user.ID, id)) {
			// Same answer for "missing" and "not yours", so ids cannot be probed.
			h.writeError(w, http.StatusNotFound, "booking not found")
			return
		}
		h.writeFullBooking(w, r, b)
	})(w, r)
}

func (h *Handler) writeFullBooking(w http.ResponseWriter, r *http.Request, b *booking.Booking) {
	j := toBookingJSON(b)
	if err := h.applySnapshot(r.Context(), &j, allIncludes); err != nil {
		h.logger.ErrorContext(r.Context(), "get booking: snapshot", "error", err, "booking_id", b.ID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, j)
}

// parseUpdatedSince reads updated_since (RFC3339).
func parseUpdatedSince(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("updated_since must be RFC3339 (e.g. 2026-10-07T00:00:00Z)")
	}
	return t, nil
}
