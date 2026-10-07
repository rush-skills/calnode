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
// links, when set, adds the booker's reschedule and cancel links above the Booking ID.
// When Calnode sends no email, the calendar invite is the only message the booker
// gets, so this is their only way back to the booking. See calendarManageLinks.
//
// plain is for providers and formats that take text (CalDAV, .ics, calendar deep
// links); rich is the same content as HTML for Google and Microsoft, or "" when there
// is neither message nor answers, so those providers fall back to plain and the event
// looks exactly as it did before. Answers are booker input and labels admin input:
// both are escaped into the HTML form and never sanitized-as-HTML. The message is
// sanitized again here: a row written by an older version or straight into the table
// is held to the allowlist.
func calendarDescription(loc *i18n.Locale, messageHTML string, answers []answerLine, links manageLinks, bookingID string) (plain, rich string) {
	idLine := loc.Tf("calendar_event_booking_id", bookingID)
	msg := richtext.Sanitize(messageHTML)
	if msg == "" && len(answers) == 0 && links.empty() {
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
	if !links.empty() {
		plainParts = append(plainParts, strings.Join([]string{
			loc.T("calendar_event_manage_intro"),
			loc.Tf("calendar_event_reschedule_line", links.Reschedule),
			loc.Tf("calendar_event_cancel_line", links.Cancel),
		}, "\n"))
		richParts = append(richParts, "<p>"+html.EscapeString(loc.T("calendar_event_manage_intro"))+" "+
			`<a href="`+html.EscapeString(links.Reschedule)+`">`+html.EscapeString(loc.T("reschedule"))+`</a> · `+
			`<a href="`+html.EscapeString(links.Cancel)+`">`+html.EscapeString(loc.T("cancel_booking"))+`</a></p>`)
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

// manageLinks are a booking's reschedule and cancel links for its calendar event.
type manageLinks struct {
	Reschedule, Cancel string
}

func (l manageLinks) empty() bool { return l.Reschedule == "" || l.Cancel == "" }

// calendarManageLinks issues a calendar-purpose manage token for the booking and
// returns the two deep links into the manage page. One token backs both links, and
// it is not rotated by a reschedule (booking.ManageTokenCalendar), because the event
// description is not rewritten then. On failure it logs and returns no links: the
// calendar event is still worth creating without them.
//
// Anyone the event is shared with can follow these links: the booker, the hosts and
// the workspace's default participants. That is the same audience that can already
// see the meeting, and the manage page only offers what the booker could do anyway.
func (h *Handler) calendarManageLinks(ctx context.Context, bookingID string) manageLinks {
	// A calendar client cannot resolve a relative link, so with no absolute public
	// URL (BASE_URL unset) there is nothing useful to write, and no token is minted.
	base := h.publicURL()
	if h.bookingSvc == nil || !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return manageLinks{}
	}
	tok, err := h.bookingSvc.IssueCalendarManageToken(ctx, bookingID)
	if err != nil {
		h.logger.ErrorContext(ctx, "calendar manage links: issue token", "error", err, "booking_id", bookingID)
		return manageLinks{}
	}
	link := base + "/manage/" + tok
	return manageLinks{Reschedule: link + "?action=reschedule", Cancel: link + "?action=cancel"}
}
