package handler

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/slots"
)

func TestNormalizeAttendeeEmails(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string // substring of the error; "" means success
	}{
		{"nil stays empty", nil, []string{}, ""},
		{"trim and lowercase", []string{"  Notes@Example.COM "}, []string{"notes@example.com"}, ""},
		{"dedupe after normalising", []string{"a@x.test", "A@X.TEST", "b@x.test"}, []string{"a@x.test", "b@x.test"}, ""},
		{"blank entries skipped", []string{"", "  ", "a@x.test"}, []string{"a@x.test"}, ""},
		{"plus and subdomain", []string{"bot+notes@mail.example.co.uk"}, []string{"bot+notes@mail.example.co.uk"}, ""},
		{"not an address", []string{"ok@x.test", "nope"}, nil, `"nope" is not a valid email address`},
		{"display name refused", []string{"Bot <bot@x.test>"}, nil, `"Bot <bot@x.test>" is not`},
		{"angle brackets refused", []string{"<bot@x.test>"}, nil, `"<bot@x.test>" is not`},
		{"two addresses in one entry", []string{"a@x.test, b@x.test"}, nil, `"a@x.test, b@x.test" is not`},
		{"missing domain", []string{"bot@"}, nil, `"bot@" is not`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeAttendeeEmails(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v; want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v; want %v", got, c.want)
			}
		})
	}

	t.Run("max 20 after dedupe", func(t *testing.T) {
		var in []string
		for i := 0; i < 21; i++ {
			in = append(in, strings.Repeat("x", 1)+string(rune('a'+i))+"@x.test")
		}
		if _, err := normalizeAttendeeEmails(in); err == nil || !strings.Contains(err.Error(), "at most 20") {
			t.Fatalf("21 entries: err = %v; want the limit named", err)
		}
		// Duplicates do not count: 21 raw entries that collapse to 20 are fine.
		in[20] = strings.ToUpper(in[0])
		if got, err := normalizeAttendeeEmails(in); err != nil || len(got) != 20 {
			t.Fatalf("21 raw / 20 unique: got %d, err %v; want 20 and no error", len(got), err)
		}
	})
}

func TestExtraAttendeesFor(t *testing.T) {
	defaults := []string{"notes@x.test", "bot@x.test", "host@x.test"}
	if got := extraAttendeesFor(nil, "a@x.test"); got != nil {
		t.Errorf("no defaults: got %v; want nil", got)
	}
	if got := extraAttendeesFor(defaults); !reflect.DeepEqual(got, defaults) {
		t.Errorf("nothing to exclude: got %v; want all", got)
	}
	// Case-insensitive: the booker and host addresses are stored as typed.
	got := extraAttendeesFor(defaults, "Host@X.test", "", "Bot@x.test")
	if want := []string{"notes@x.test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
	// Everything excluded collapses to nil, not an empty slice, so a provider request is
	// byte-identical to the pre-feature one.
	if got := extraAttendeesFor(defaults, defaults...); got != nil {
		t.Errorf("all excluded: got %#v; want nil", got)
	}
}

// A database with no settings row (older than migration 00011, or never set up) is "no
// defaults", not an error - nothing in the booking path may fail on it.
func TestDefaultAttendeeEmails_missingRowIsNil(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DELETE FROM server_settings`); err != nil {
		t.Fatal(err)
	}
	h := New(database, slog.Default())
	got, err := h.defaultAttendeeEmails(context.Background())
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nil, nil", got, err)
	}

	// With the row present and the column set, it splits and skips blanks.
	if _, err := database.Exec(`INSERT INTO server_settings (id, default_attendee_emails) VALUES (1, 'a@x.test,,b@x.test')`); err != nil {
		t.Fatal(err)
	}
	got, err = h.defaultAttendeeEmails(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"a@x.test", "b@x.test"}) {
		t.Fatalf("got %v, %v; want [a b], nil", got, err)
	}
}

// recordingProvider captures the params of every CreateEvent so the reconcile test can see
// what a healed event would carry. Reports a destination so reconcileCreations does not skip.
type recordingProvider struct {
	stuckProvider
	params []calendar.CreateEventParams
}

func (p *recordingProvider) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *recordingProvider) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *recordingProvider) CreateEvent(_ context.Context, _ string, params calendar.CreateEventParams) (string, string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.params = append(p.params, params)
	return "healed-ev", "", "cal", nil
}

// A healed (reconciled) event carries the default participants, minus the host and the booker.
func TestReconcileCreations_healedEventCarriesDefaultParticipants(t *testing.T) {
	var logs bytes.Buffer
	h, database, _ := newReconcileFixture(t, "confirmed", &logs)
	for _, q := range []string{
		// The fixture's host row has an event id; make it a missing one, old enough to heal.
		`UPDATE booking_hosts SET external_event_id = NULL, needs_sync = 0 WHERE id = 'bh-1'`,
		`UPDATE bookings SET created_at = '2020-01-01T00:00:00Z' WHERE id = 'bk-1'`,
		`INSERT INTO booking_attendees (id, booking_id, name, email, is_organizer) VALUES ('at-1', 'bk-1', 'Alice', 'Alice@example.com', 1)`,
		`INSERT INTO calendar_connections (id, user_id, provider, access_token_enc, calendar_id, is_destination) VALUES ('c1', 'host-1', 'caldav', 'x', 'cal', 1)`,
		// A co-host on the same booking whose own event already exists: they are on the
		// meeting, so they must not be invited to host-1's healed event either.
		`INSERT INTO users (id, email, name, iana_timezone) VALUES ('host-2', 'cohost@example.com', 'Co', 'UTC')`,
		`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary, external_event_id) VALUES ('bh-2', 'bk-1', 'host-2', 0, 'ev-2')`,
		`UPDATE server_settings SET default_attendee_emails = 'notes@example.com,host@example.com,Alice@example.com,cohost@example.com' WHERE id = 1`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	p := &recordingProvider{}
	svc := calendar.NewService(database)
	svc.Register(p)

	h.reconcileCreations(context.Background(), svc)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.params) != 1 {
		t.Fatalf("CreateEvent called %d times; want 1 (logs: %s)", len(p.params), logs.String())
	}
	if want := []string{"notes@example.com"}; !reflect.DeepEqual(p.params[0].ExtraAttendees, want) {
		t.Fatalf("ExtraAttendees = %v; want %v (host, co-host and booker excluded)", p.params[0].ExtraAttendees, want)
	}
	eventID, _ := hostRow(t, database)
	if eventID.String != "healed-ev" {
		t.Errorf("healed event id not stored: %v", eventID)
	}
}
