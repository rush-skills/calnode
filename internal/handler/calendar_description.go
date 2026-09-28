package handler

import (
	"context"
	"html"
	"strings"

	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/richtext"
)

// answerLine is one booking-question answer for the calendar description: the
// question's label and the booker's answer, in the questions' configured order.
type answerLine struct {
	Label, Value string
}

// loadAnswerLines returns the booking's answers to the event type's questions, in
// question order, skipping blank answers. An error is logged by the caller as a
// degraded invite, never a failed one: the booking exists whether or not the host
// sees the answers on the event.
func (h *Handler) loadAnswerLines(ctx context.Context, bookingID string) ([]answerLine, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT q.label, a.value
		FROM booking_answers a
		JOIN event_type_questions q ON q.id = a.question_id
		WHERE a.booking_id = ?
		ORDER BY q.position, q.id`, bookingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []answerLine
	for rows.Next() {
		var l answerLine
		if err := rows.Scan(&l.Label, &l.Value); err != nil {
			return nil, err
		}
		if strings.TrimSpace(l.Value) == "" {
			continue
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// calendarDescription composes the body of a booking's calendar event: the event
// type's calendar message (admin-authored, stored as sanitized HTML), then the
// booker's answers to the booking questions ("Label: answer", one per line, so the
// host has the context on the event itself), then the translated "Booking ID: …"
// line, which stays LAST and always present — the reconcile sweep and support match
// events on it. It is the ONE place this text is built; the three creation sites
// (inline, reconcile, reassign) all call it so they cannot drift.
//
// plain is for providers and formats that take text (CalDAV, .ics, calendar deep
// links); rich is the same content as HTML for Google and Microsoft, or "" when there
// is neither message nor answers, so those providers fall back to plain and the event
// looks exactly as it did before. Answers are booker input and labels admin input:
// both are escaped into the HTML form and never sanitized-as-HTML. The message is
// sanitized again here: a row written by an older version or straight into the table
// is held to the allowlist.
func calendarDescription(loc *i18n.Locale, messageHTML string, answers []answerLine, bookingID string) (plain, rich string) {
	idLine := loc.Tf("calendar_event_booking_id", bookingID)
	msg := richtext.Sanitize(messageHTML)
	if msg == "" && len(answers) == 0 {
		return idLine, ""
	}
	var plainParts, richParts []string
	if msg != "" {
		plainParts = append(plainParts, richtext.ToPlainText(msg))
		richParts = append(richParts, msg)
	}
	if len(answers) > 0 {
		var pl, rp []string
		for _, a := range answers {
			pl = append(pl, a.Label+": "+a.Value)
			rp = append(rp, "<p><strong>"+html.EscapeString(a.Label)+":</strong> "+html.EscapeString(a.Value)+"</p>")
		}
		plainParts = append(plainParts, strings.Join(pl, "\n"))
		richParts = append(richParts, strings.Join(rp, ""))
	}
	plainParts = append(plainParts, idLine)
	richParts = append(richParts, "<p>"+html.EscapeString(idLine)+"</p>")
	return strings.Join(plainParts, "\n\n"), strings.Join(richParts, "")
}

// calendarMessageText is the plain-text calendar message alone (no Booking ID line),
// for the attendee's .ics attachment and "add to calendar" links, where the ID would
// only add noise — the attendee's own confirmation email already carries it.
func calendarMessageText(messageHTML string) string {
	return richtext.ToPlainText(richtext.Sanitize(messageHTML))
}
