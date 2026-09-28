package handler

import (
	"html"

	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/richtext"
)

// calendarDescription composes the body of a booking's calendar event: the event
// type's calendar message (admin-authored, stored as sanitized HTML) followed by the
// translated "Booking ID: …" line, which stays LAST and always present — the
// reconcile sweep and support match events on it. It is the ONE place this text is
// built; the three creation sites (inline, reconcile, reassign) all call it so they
// cannot drift.
//
// plain is for providers and formats that take text (CalDAV, .ics, calendar deep
// links); rich is the same content as HTML for Google and Microsoft, or "" when there
// is no message, so those providers fall back to plain and the event looks exactly as
// it did before the message existed. The message is sanitized again here: a row
// written by an older version or straight into the table is held to the allowlist.
func calendarDescription(loc *i18n.Locale, messageHTML, bookingID string) (plain, rich string) {
	idLine := loc.Tf("calendar_event_booking_id", bookingID)
	msg := richtext.Sanitize(messageHTML)
	if msg == "" {
		return idLine, ""
	}
	plain = richtext.ToPlainText(msg) + "\n\n" + idLine
	rich = msg + "<p>" + html.EscapeString(idLine) + "</p>"
	return plain, rich
}

// calendarMessageText is the plain-text calendar message alone (no Booking ID line),
// for the attendee's .ics attachment and "add to calendar" links, where the ID would
// only add noise — the attendee's own confirmation email already carries it.
func calendarMessageText(messageHTML string) string {
	return richtext.ToPlainText(richtext.Sanitize(messageHTML))
}
