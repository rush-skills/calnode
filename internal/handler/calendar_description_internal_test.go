package handler

import (
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

func TestCalendarDescription_noMessageIsJustTheBookingIDLine(t *testing.T) {
	plain, rich := calendarDescription(nil, "", "bk-1")
	if plain != "Booking ID: bk-1" {
		t.Errorf("plain = %q", plain)
	}
	if rich != "" {
		t.Errorf("rich = %q; want empty so providers fall back to plain", rich)
	}
	// Markup that sanitizes to nothing counts as no message.
	plain, rich = calendarDescription(nil, "<script>x()</script><p></p>", "bk-1")
	if plain != "Booking ID: bk-1" || rich != "" {
		t.Errorf("structural-only message: plain=%q rich=%q", plain, rich)
	}
}

func TestCalendarDescription_messageFirstBookingIDLast(t *testing.T) {
	msg := `<h2>Agenda</h2><p>Bring <b>questions</b>.<br>See <a href="https://x.io/p">prep</a>.</p><ul><li>One</li></ul><img src=x onerror=alert(1)>`
	plain, rich := calendarDescription(i18n.Default(), msg, "bk-<1>")

	wantPlain := "Agenda\n\nBring questions.\nSee prep (https://x.io/p).\n\n- One\n\nBooking ID: bk-<1>"
	if plain != wantPlain {
		t.Errorf("plain =\n%q\nwant\n%q", plain, wantPlain)
	}
	if !strings.HasSuffix(rich, "<p>Booking ID: bk-&lt;1&gt;</p>") {
		t.Errorf("rich should end with the escaped Booking ID paragraph: %q", rich)
	}
	if !strings.HasPrefix(rich, "<h2>Agenda</h2>") || strings.Contains(rich, "<img") || strings.Contains(rich, "onerror") {
		t.Errorf("rich should be the sanitized message first: %q", rich)
	}
}

func TestCalendarDescription_bookingIDLineIsTranslated(t *testing.T) {
	es := i18n.Get("es")
	if es == nil {
		t.Skip("es locale not shipped")
	}
	plain, rich := calendarDescription(es, "<p>Hola</p>", "bk-2")
	want := es.Tf("calendar_event_booking_id", "bk-2")
	if !strings.HasSuffix(plain, "\n\n"+want) || !strings.HasSuffix(rich, "<p>"+want+"</p>") {
		t.Errorf("plain=%q rich=%q; want both to end with %q", plain, rich, want)
	}
}

func TestCalendarMessageText_plainTextWithoutBookingID(t *testing.T) {
	if got := calendarMessageText("<p>Agenda &amp; <em>notes</em></p><script>x</script>"); got != "Agenda & notes" {
		t.Errorf("got %q", got)
	}
	if got := calendarMessageText(""); got != "" {
		t.Errorf("empty message should give empty text, got %q", got)
	}
}
