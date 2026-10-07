// Package bookingsnap reads one booking in the full shape an external system needs:
// its meeting, every host, every attendee and its answers keyed by question id. The
// webhook payload and GET /v1/bookings share it, so a receiver that repairs its copy
// from the API gets exactly what the webhooks sent (docs/webhooks.md).
package bookingsnap

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
)

// ErrNotFound is returned when no booking has the id.
var ErrNotFound = errors.New("bookingsnap: booking not found")

// Host is one host on the booking. Role is "primary" (leads the meeting), "fixed" (a
// required co-host), "rotation" or "optional", taken from the event type's host list.
type Host struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// Attendee is one attendee. Phone is set when the booker chose a phone call.
type Attendee struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Timezone   string `json:"timezone"`
	Phone      string `json:"phone,omitempty"`
	Locale     string `json:"locale"`
	RSVPStatus string `json:"rsvp_status"`
	Organizer  bool   `json:"organizer"`
}

// Answer is one intake answer, keyed by the question's id so renaming a question does
// not break a receiver's mapping.
type Answer struct {
	QuestionID string `json:"question_id"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
}

// Meeting is where the meeting happens and how to find its calendar event.
type Meeting struct {
	// Provider is google_meet, teams, zoom, livekit, phone, in_person or manual (a link
	// the organizer typed, including a Meet/Teams type whose link was not generated).
	Provider string `json:"provider"`
	// JoinURL is the attendee's link. For LiveKit it is the attendee room link; the
	// host's controls link never leaves Calnode.
	JoinURL string `json:"join_url"`
	// CalendarProvider is google, microsoft, caldav, or none when no calendar event exists.
	CalendarProvider string `json:"calendar_provider"`
	CalendarEventID  string `json:"calendar_event_id"`
	// ICalUID is the iCalendar UID of the primary host's event, the id note takers use.
	ICalUID string `json:"ical_uid"`
}

// Snapshot is a booking as it stands now.
type Snapshot struct {
	ID                 string
	Status             string
	StartAt, EndAt     string
	CreatedAt          string
	ChangedAt          string
	Revision           int
	CancellationReason string
	LocationValue      string
	EventTypeID        string
	EventTypeSlug      string
	EventTypeName      string
	HostID             string
	HostName           string
	HostEmail          string
	Hosts              []Host
	Attendees          []Attendee
	Answers            []Answer
	Meeting            Meeting
	AdminURL           string
}

// Load reads booking id. baseURL (the instance's public URL) builds AdminURL; empty
// leaves it empty. Queries run one after another and drain their rows, which the
// single-connection pool requires.
func Load(ctx context.Context, db *sql.DB, id, baseURL string) (*Snapshot, error) {
	s := &Snapshot{ID: id, Hosts: []Host{}, Attendees: []Attendee{}, Answers: []Answer{}}
	var bookingLocType, etLocType string
	err := db.QueryRowContext(ctx, `
		SELECT b.status, b.start_at, b.end_at, b.created_at, b.changed_at, b.revision,
		       COALESCE(b.cancellation_reason, ''), COALESCE(b.location_value, ''),
		       COALESCE(b.location_type, ''), et.id, et.slug, et.name, COALESCE(et.location_type, ''),
		       b.host_id, COALESCE(u.name, ''), COALESCE(u.email, ''), COALESCE(b.external_event_id, '')
		FROM bookings b
		JOIN event_types et ON et.id = b.event_type_id
		LEFT JOIN users u ON u.id = b.host_id
		WHERE b.id = ?`, id).Scan(&s.Status, &s.StartAt, &s.EndAt, &s.CreatedAt, &s.ChangedAt, &s.Revision,
		&s.CancellationReason, &s.LocationValue, &bookingLocType, &s.EventTypeID, &s.EventTypeSlug,
		&s.EventTypeName, &etLocType, &s.HostID, &s.HostName, &s.HostEmail, &s.Meeting.CalendarEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if baseURL != "" {
		s.AdminURL = strings.TrimRight(baseURL, "/") + "/admin/bookings/" + url.PathEscape(id)
	}
	locType := bookingLocType
	if locType == "" {
		locType = etLocType
	}

	// Hosts, primary first. Role comes from the event type's host list; a host who is
	// not on it any more (removed since) is reported as fixed.
	rows, err := db.QueryContext(ctx, `
		SELECT bh.user_id, COALESCE(u.name, ''), COALESCE(u.email, ''), bh.is_primary,
		       COALESCE(eth.role, ''), COALESCE(bh.external_provider, ''),
		       COALESCE(bh.external_event_id, ''), bh.external_ical_uid
		FROM booking_hosts bh
		LEFT JOIN users u ON u.id = bh.user_id
		LEFT JOIN event_type_hosts eth ON eth.event_type_id = ? AND eth.user_id = bh.user_id
		WHERE bh.booking_id = ?
		ORDER BY bh.is_primary DESC, u.name`, s.EventTypeID, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h Host
		var primary int
		var etRole, provider, eventID, icalUID string
		if err := rows.Scan(&h.ID, &h.Name, &h.Email, &primary, &etRole, &provider, &eventID, &icalUID); err != nil {
			rows.Close() // #nosec G104 -- returning the scan error
			return nil, err
		}
		switch {
		case primary == 1:
			h.Role = "primary"
			if eventID != "" {
				s.Meeting.CalendarEventID = eventID
				s.Meeting.CalendarProvider = provider
				s.Meeting.ICalUID = icalUID
			}
		case etRole == "rotation" || etRole == "optional":
			h.Role = etRole
		default:
			h.Role = "fixed"
		}
		s.Hosts = append(s.Hosts, h)
	}
	rows.Close() // #nosec G104 -- drained
	if len(s.Hosts) == 0 && s.HostID != "" {
		s.Hosts = append(s.Hosts, Host{ID: s.HostID, Name: s.HostName, Email: s.HostEmail, Role: "primary"})
	}

	rows, err = db.QueryContext(ctx, `
		SELECT name, email, COALESCE(iana_timezone, ''), COALESCE(locale, ''), rsvp_status, is_organizer
		FROM booking_attendees WHERE booking_id = ?
		ORDER BY is_organizer DESC, name`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Attendee
		var org int
		if err := rows.Scan(&a.Name, &a.Email, &a.Timezone, &a.Locale, &a.RSVPStatus, &org); err != nil {
			rows.Close() // #nosec G104 -- returning the scan error
			return nil, err
		}
		a.Organizer = org == 1
		if a.Organizer && locType == "phone" && strings.HasPrefix(s.LocationValue, "tel:") {
			a.Phone = strings.TrimSpace(strings.TrimPrefix(s.LocationValue, "tel:"))
		}
		s.Attendees = append(s.Attendees, a)
	}
	rows.Close() // #nosec G104 -- drained

	rows, err = db.QueryContext(ctx, `
		SELECT q.id, q.label, ba.value
		FROM booking_answers ba
		JOIN event_type_questions q ON q.id = ba.question_id
		WHERE ba.booking_id = ?
		ORDER BY q.position, q.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Answer
		if err := rows.Scan(&a.QuestionID, &a.Question, &a.Answer); err != nil {
			rows.Close() // #nosec G104 -- returning the scan error
			return nil, err
		}
		s.Answers = append(s.Answers, a)
	}
	rows.Close() // #nosec G104 -- drained

	s.Meeting.Provider, s.Meeting.JoinURL = meetingProvider(locType, s.LocationValue)
	if s.Meeting.CalendarProvider == "" {
		s.Meeting.CalendarProvider = "none"
	}
	return s, nil
}

// meetingProvider names the meeting platform for a location type and value. A Meet or
// Teams type whose stored link is not on that platform's host was a manual fallback
// link (the host's calendar could not generate one), so it is reported as manual.
func meetingProvider(locType, value string) (provider, joinURL string) {
	host := ""
	if u, err := url.Parse(value); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	switch locType {
	case "google_meet":
		if host == "meet.google.com" {
			return "google_meet", value
		}
		return "manual", value
	case "teams":
		if strings.HasSuffix(host, "teams.microsoft.com") || strings.HasSuffix(host, "teams.live.com") {
			return "teams", value
		}
		return "manual", value
	case "zoom", "livekit":
		return locType, value
	case "phone":
		return "phone", ""
	case "in_person":
		return "in_person", ""
	default:
		return "manual", value
	}
}
