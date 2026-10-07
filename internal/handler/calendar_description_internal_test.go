package handler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/i18n"
)

func TestCalendarDescription_noMessageIsJustTheBookingIDLine(t *testing.T) {
	plain, rich := calendarDescription(nil, "", nil, manageLinks{}, "bk-1")
	if plain != "Booking ID: bk-1" {
		t.Errorf("plain = %q", plain)
	}
	if rich != "" {
		t.Errorf("rich = %q; want empty so providers fall back to plain", rich)
	}
	// Markup that sanitizes to nothing counts as no message.
	plain, rich = calendarDescription(nil, "<script>x()</script><p></p>", nil, manageLinks{}, "bk-1")
	if plain != "Booking ID: bk-1" || rich != "" {
		t.Errorf("structural-only message: plain=%q rich=%q", plain, rich)
	}
}

func TestCalendarDescription_messageFirstBookingIDLast(t *testing.T) {
	msg := `<h2>Agenda</h2><p>Bring <b>questions</b>.<br>See <a href="https://x.io/p">prep</a>.</p><ul><li>One</li></ul><img src=x onerror=alert(1)>`
	plain, rich := calendarDescription(i18n.Default(), msg, nil, manageLinks{}, "bk-<1>")

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
	plain, rich := calendarDescription(es, "<p>Hola</p>", nil, manageLinks{}, "bk-2")
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

func TestCalendarDescription_answersBetweenMessageAndBookingID(t *testing.T) {
	answers := []answerLine{{"Company", "Acme <Ltd>"}, {"Agree to terms", "yes"}}
	plain, rich := calendarDescription(nil, "<p>Agenda</p>", answers, manageLinks{}, "bk-3")
	if plain != "Agenda\n\nCompany: Acme <Ltd>\nAgree to terms: yes\n\nBooking ID: bk-3" {
		t.Errorf("plain = %q", plain)
	}
	if rich != "<p>Agenda</p><p><strong>Company:</strong> Acme &lt;Ltd&gt;</p><p><strong>Agree to terms:</strong> yes</p><p>Booking ID: bk-3</p>" {
		t.Errorf("rich = %q", rich)
	}

	// Answers without a message still produce both forms.
	plain, rich = calendarDescription(nil, "", answers[:1], manageLinks{}, "bk-4")
	if plain != "Company: Acme <Ltd>\n\nBooking ID: bk-4" || rich != "<p><strong>Company:</strong> Acme &lt;Ltd&gt;</p><p>Booking ID: bk-4</p>" {
		t.Errorf("answers only: plain=%q rich=%q", plain, rich)
	}
}

// The loader degrades, never crashes: a query error surfaces as an error the caller
// logs, and a row that cannot be scanned does the same. Both are reached here by
// tampering with the table under the handler, which is the only way to make SQLite
// misbehave on demand.
func TestLoadAnswerLines_errorsSurface(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := New(database, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The API never stores a blank answer, but a row written directly can be blank;
	// the loader skips it rather than putting "Label: " on the invite.
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`DROP TABLE booking_answers`,
		`CREATE TABLE booking_answers (id TEXT PRIMARY KEY, booking_id TEXT, question_id TEXT, value TEXT)`,
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('et', 'u', 's', 'n', 30)`,
		`INSERT INTO event_type_questions (id, event_type_id, label, type, position) VALUES ('q', 'et', 'Label', 'text', 0)`,
		`INSERT INTO event_type_questions (id, event_type_id, label, type, position) VALUES ('q2', 'et', 'Blank', 'text', 1)`,
		`INSERT INTO booking_answers (id, booking_id, question_id, value) VALUES ('a', 'b', 'q', 'Pricing')`,
		`INSERT INTO booking_answers (id, booking_id, question_id, value) VALUES ('a2', 'b', 'q2', '   ')`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	lines, err := h.loadAnswerLines(context.Background(), "b")
	if err != nil || len(lines) != 1 || lines[0] != (answerLine{"Label", "Pricing"}) {
		t.Errorf("lines = %v (err %v); want just Label: Pricing", lines, err)
	}

	// A NULL value where a string is expected: the scan fails.
	if _, err := database.Exec(`INSERT INTO booking_answers (id, booking_id, question_id, value) VALUES ('a0', 'b', 'q', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.loadAnswerLines(context.Background(), "b"); err == nil {
		t.Error("scan of a NULL answer should error")
	}

	// No table at all: the query itself fails.
	if _, err := database.Exec(`DROP TABLE booking_answers`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.loadAnswerLines(context.Background(), "b"); err == nil {
		t.Error("query against a missing table should error")
	}
}

// The manage links sit between the answers and the Booking ID, which stays last, and
// are escaped into the HTML form.
func TestCalendarDescription_manageLinks(t *testing.T) {
	links := manageLinks{Reschedule: "https://b.example/manage/t?action=reschedule", Cancel: "https://b.example/manage/t?action=cancel&x=\"y\""}
	plain, rich := calendarDescription(nil, "", nil, links, "bk-9")
	want := "Need to make a change?\nReschedule: " + links.Reschedule + "\nCancel: " + links.Cancel + "\n\nBooking ID: bk-9"
	if plain != want {
		t.Errorf("plain = %q; want %q", plain, want)
	}
	if !strings.Contains(rich, `<a href="https://b.example/manage/t?action=cancel&amp;x=&#34;y&#34;">Cancel booking</a>`) ||
		!strings.HasSuffix(rich, "<p>Booking ID: bk-9</p>") {
		t.Errorf("rich = %q", rich)
	}
	// Half a pair is no pair: nothing is written.
	if plain, _ := calendarDescription(nil, "", nil, manageLinks{Reschedule: "x"}, "bk-9"); plain != "Booking ID: bk-9" {
		t.Errorf("partial links: plain = %q", plain)
	}
}
