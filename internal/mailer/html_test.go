package mailer

import (
	"context"
	"strings"
	"testing"
	"time"
)

func sampleBookingData() BookingData {
	start := time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC) // 9am NZST
	return BookingData{
		BookingID:         "abc-123",
		EventTypeName:     "20-minute call",
		EventTypeSlug:     "intro",
		HostName:          "Wynne Pirini",
		HostEmail:         "host@example.com",
		OrganizerName:     "Alex Johnson",
		OrganizerEmail:    "alex@example.com",
		OrganizerTimezone: "Pacific/Auckland",
		StartAt:           start,
		EndAt:             start.Add(20 * time.Minute),
		PreviousStartAt:   start.AddDate(0, 0, -1),
		PreviousEndAt:     start.AddDate(0, 0, -1).Add(20 * time.Minute),
		ManageURL:         "https://booking.example.com/manage/tok",
		BaseURL:           "https://booking.example.com",
		BrandName:         "Orchestratr",
	}
}

// Every HTML template must render to non-empty output (renderHTML returns "" on
// any execution error, so empty means a broken template).
func TestRenderHTML_allTemplates(t *testing.T) {
	d := sampleBookingData()
	cases := map[string]string{
		"confirm-org":     renderHTML(htmlConfirmOrg, d),
		"confirm-host":    renderHTML(htmlConfirmHost, d),
		"cancel-org":      renderHTML(htmlCancelOrg, d),
		"cancel-host":     renderHTML(htmlCancelHost, d),
		"reschedule-org":  renderHTML(htmlRescheduleOrg, d),
		"reschedule-host": renderHTML(htmlRescheduleHost, d),
		"reminder-org":    renderHTML(htmlReminderOrg, d),
	}
	for name, out := range cases {
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s: rendered empty HTML (template error)", name)
			continue
		}
		if !strings.Contains(out, "20-minute call") {
			t.Errorf("%s: missing event name", name)
		}
		// No logo set → no logo header bar, and no repeated brand wordmark in body.
		if strings.Contains(out, "<img") {
			t.Errorf("%s: rendered a logo <img> with no LogoURL set", name)
		}
	}

	// Attendee confirmation must carry the calendar buttons and manage link.
	conf := cases["confirm-org"]
	if !strings.Contains(conf, "calendar.google.com") || !strings.Contains(conf, "outlook.office.com") {
		t.Error("confirm-org: missing add-to-calendar links")
	}
	if !strings.Contains(conf, "/manage/tok") {
		t.Error("confirm-org: missing manage link")
	}

	// With a logo, the header renders the image with the brand as alt text.
	d.LogoURL = "https://cdn.example.com/logo.png"
	withLogo := renderHTML(htmlConfirmOrg, d)
	if !strings.Contains(withLogo, `src="https://cdn.example.com/logo.png"`) {
		t.Error("confirm-org: logo image not rendered when LogoURL set")
	}
	if !strings.Contains(withLogo, `alt="Orchestratr"`) {
		t.Error("confirm-org: logo alt should be the brand name")
	}

	// With a banner, it renders full-width below the logo, with no border/padding.
	d.BannerURL = "https://cdn.example.com/banner.png"
	d.BannerOpacity = 60
	withBanner := renderHTML(htmlConfirmOrg, d)
	if !strings.Contains(withBanner, `src="https://cdn.example.com/banner.png"`) {
		t.Error("confirm-org: banner image not rendered when BannerURL set")
	}
	if !strings.Contains(withBanner, `opacity:0.6`) {
		t.Error("confirm-org: banner opacity not applied")
	}
	if strings.Index(withBanner, "cdn.example.com/logo.png") > strings.Index(withBanner, "cdn.example.com/banner.png") {
		t.Error("confirm-org: banner should render after the logo, not before")
	}
}

func TestBookingData_Brand(t *testing.T) {
	if got := (BookingData{}).Brand(); got != "Calnode" {
		t.Errorf("Brand() empty = %q; want Calnode", got)
	}
	if got := (BookingData{BrandName: "Acme"}).Brand(); got != "Acme" {
		t.Errorf("Brand() set = %q; want Acme", got)
	}
}

// Email notes are rich text: the text part gets readable plain text, the HTML part
// the allowlisted markup, and anything dangerous is stripped on send even if it
// somehow reached the row. A legacy plain-text note keeps its line breaks.
func TestCustomNote_richTextRendersInBothParts(t *testing.T) {
	d := testBookingData()
	d.CustomNote = `<p>Bring <strong>ID</strong>.</p><ul><li>Park at <a href="https://x.io/p">the lot</a></li></ul><script>alert(1)</script>`

	if got, want := d.CustomNoteText(), "Bring ID.\n\n- Park at the lot (https://x.io/p)"; got != want {
		t.Errorf("CustomNoteText = %q; want %q", got, want)
	}
	html := string(d.CustomNoteHTML())
	if !strings.Contains(html, "<strong>ID</strong>") || strings.Contains(html, "<script") {
		t.Errorf("CustomNoteHTML = %q", html)
	}

	// Every attendee-facing template appends the note in both parts.
	for name, send := range map[string]func(context.Context, Mailer, BookingData) error{
		"confirmation": SendConfirmationToAttendee,
		"cancellation": SendCancellationToAttendee,
		"reschedule":   SendRescheduleToAttendee,
		"reminder":     SendReminder,
	} {
		cap := &captureMailer{}
		if err := send(context.Background(), cap, d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		msg := cap.all()[0]
		if !strings.Contains(msg.Text, "- Park at the lot (https://x.io/p)") || strings.Contains(msg.Text, "<strong>") {
			t.Errorf("%s text part: %q", name, msg.Text)
		}
		if !strings.Contains(msg.HTML, "<strong>ID</strong>") || strings.Contains(msg.HTML, "&lt;strong&gt;") || strings.Contains(msg.HTML, "<script") {
			t.Errorf("%s html part should carry the sanitized markup unescaped: %s", name, msg.HTML)
		}
	}

	// A note written before the rich editor: plain text, line breaks kept, and the
	// ampersand escaped in the HTML part but not the text part.
	legacy := testBookingData()
	legacy.CustomNote = "Line one\nQ & A"
	if got := legacy.CustomNoteText(); got != "Line one\nQ & A" {
		t.Errorf("legacy text = %q", got)
	}
	if got := string(legacy.CustomNoteHTML()); got != "Line one\nQ &amp; A" {
		t.Errorf("legacy html = %q", got)
	}
}
