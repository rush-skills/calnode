// Package calendar abstracts external calendar providers (Google, Microsoft, …)
// behind a single Provider interface and a Service that routes per-user by the
// provider stored on their calendar_connections row. The booking/slot/reconciler
// code talks only to *Service and never to a concrete provider.
package calendar

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/calnode/calnode/internal/slots"
)

// CreateEventParams holds the data needed to create a calendar event. Provider-
// agnostic: AddMeet requests the provider's online-meeting (Google Meet / Teams).
type CreateEventParams struct {
	Summary     string
	Description string // plain text; every provider can carry this
	// DescriptionHTML is the same content as rich HTML for providers that render it
	// (Google, Microsoft). Empty means "use Description". CalDAV ignores it: an .ics
	// DESCRIPTION is text.
	DescriptionHTML string
	Location        string // optional; e.g. the meeting link on secondary hosts' events
	Start, End      time.Time
	OrganizerName   string
	OrganizerEmail  string
	AddMeet         bool
	// ExtraAttendees are additional email addresses to invite on the host's calendar event,
	// beyond the organizer (the booker). They come from the workspace's default participants
	// setting (PATCH /v1/settings/participants) and exist so a notetaker bot or a shared
	// mailbox receives the provider's native invite. Every provider adds them as required
	// attendees of the event it writes; none of them is ever emailed by Calnode itself, and
	// the booker's .ics never lists them. The caller is responsible for dedupe against the
	// organizer and host addresses - providers append what they are given.
	ExtraAttendees []string
	// ICalUID, when non-nil, receives the created event's iCalendar UID from providers that
	// report it (Google iCalUID, Graph iCalUId, CalDAV's own UID). Note takers identify a
	// meeting by it, so Calnode stores it as the join key to the booking's transcript. An
	// out-parameter rather than a fifth return value so the many Provider implementations
	// that have no use for it are untouched.
	ICalUID *string
}

// ExternalEvent is one event read back from a member's connected calendar, for the
// team calendar. Only what that view shows: no attendees, no description. Start and
// End are UTC instants; for an all-day event they are the UTC midnights bounding the
// day(s), which is how both Google ("date") and Graph (isAllDay) express them.
type ExternalEvent struct {
	ID       string
	Title    string
	Location string
	Start    time.Time
	End      time.Time
	AllDay   bool
	// Source is the provider name ("google" | "microsoft"). Providers leave it empty;
	// Service.ListEvents stamps it, so callers never need to know which backend answered.
	Source string
}

// CalendarInfo is one calendar the provider exposes for a connected account.
type CalendarInfo struct {
	ID      string `json:"id"`      // provider calendar id ("primary", an address, a URL)
	Name    string `json:"name"`    // display name
	Primary bool   `json:"primary"` // the account's default calendar
	// Writable reports whether events can be created here. A calendar shared with the user
	// read-only is perfectly valid for conflict checking but cannot be a write target, and
	// finding that out at booking time means a failed booking rather than a disabled radio.
	Writable bool `json:"writable"`
}

// CalendarSelection is a CalendarInfo plus the user's per-calendar settings.
type CalendarSelection struct {
	CalendarInfo
	CheckConflicts bool `json:"check_conflicts"` // include in free/busy
	IsDestination  bool `json:"is_destination"`  // write new events here
}

// Provider is one calendar backend for a connected user. All operations are keyed
// by userID and resolve that user's stored credentials internally; they return
// zero values (not errors) when the user has no matching connection.
type Provider interface {
	Name() string        // "google" | "microsoft"
	InvitesGuests() bool // provider emails guests itself → suppress our own .ics

	// OAuth
	AuthURL(state string) string
	EncryptState(userID string) (string, error)
	DecryptState(state string) (string, error)
	Exchange(ctx context.Context, userID, code, calendarID string) error

	// Connection state
	Connected(ctx context.Context, userID string) (bool, error)
	Disconnect(ctx context.Context, userID string) error
	HasDestination(ctx context.Context, userID string) (bool, error)

	// ListCalendars returns the calendars in one connected account (by account email).
	ListCalendars(ctx context.Context, userID, accountEmail string) ([]CalendarInfo, error)

	// Operations
	FreeBusy(ctx context.Context, userID string, from, to time.Time) ([]slots.Interval, error)

	// ListEvents returns the user's events overlapping [from, to) across the calendars
	// they selected for conflict checking - the same set FreeBusy reads, with titles. A
	// provider that cannot (or does not) read events back returns (nil, nil), never an
	// error, so a user with such a connection simply contributes nothing.
	ListEvents(ctx context.Context, userID string, from, to time.Time) ([]ExternalEvent, error)

	// CreateEvent writes to the user's destination calendar and also returns WHICH calendar
	// that was, so the caller can store it against the booking. Update and Cancel then act
	// on that stored calendar rather than re-resolving the current destination, which would
	// break every existing booking the moment a host changes their destination.
	CreateEvent(ctx context.Context, userID string, p CreateEventParams) (eventID, joinURL, calendarID string, err error)

	// calendarID is the one CreateEvent reported. Empty means "resolve the destination the
	// old way" - correct for bookings made before that was recorded. location replaces the
	// event's location when non-empty (a re-minted LiveKit join link after a reschedule);
	// "" leaves it as it is. Attendees, including ExtraAttendees, are never touched.
	UpdateEvent(ctx context.Context, userID, calendarID, eventID string, start, end time.Time, location string) error
	CancelEvent(ctx context.Context, userID, calendarID, eventID string) error
}

// EventRecognizer is implemented by a provider whose event ids identify the provider by
// themselves. The Service sends an update or cancel of such an event to that provider whatever
// the user's destination is now, so moving the destination to another provider neither strands
// the events written before the move nor hands their ids to a provider that never issued them.
//
// CalDAV implements it: its event id is the absolute URL of the event resource. Google and
// Microsoft ids are opaque and never URLs; for them the stamped provider recorded at creation
// (booking_hosts.external_provider, migration 00062) decides, falling back to recognition and
// then the destination for rows written before stamping.
type EventRecognizer interface {
	RecognizesEvent(eventID string) bool
}

// ErrEventUnreachable matches an UpdateEvent or CancelEvent error that was refused before
// anything was sent, because the stored ids do not identify one connected account to act as.
// It is a verdict on stored state, not a failed request: a retry re-reads the same connections
// and refuses the same way, so the reconciler stops retrying an event that returns it.
var ErrEventUnreachable = errors.New("calendar: no single connected account holds this event")

// Service holds the configured providers and dispatches per-user operations to
// whichever provider that user has connected.
type Service struct {
	db        *sql.DB
	providers map[string]Provider
	primary   string // default provider for new connections (first registered)
}

// NewService returns an empty Service. Register one provider per configured backend.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, providers: map[string]Provider{}}
}

// Register adds a provider (keyed by Name()); the first registered becomes primary.
func (s *Service) Register(p Provider) {
	s.providers[p.Name()] = p
	if s.primary == "" {
		s.primary = p.Name()
	}
}

// Any reports whether at least one provider is configured.
func (s *Service) Any() bool { return s != nil && len(s.providers) > 0 }

// Primary returns the default provider for new connections (nil if none).
func (s *Service) Primary() Provider { return s.providers[s.primary] }

// Provider returns the named provider, or nil.
func (s *Service) Provider(name string) Provider { return s.providers[name] }

// ProviderNames returns the configured provider names, sorted, so the UI can
// offer the right connect options.
func (s *Service) ProviderNames() []string {
	names := make([]string, 0, len(s.providers))
	for n := range s.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// providerForDestination resolves the provider of the user's DESTINATION connection — the one
// calendar bookings are written to (is_destination = 1). Write/capability ops route here.
// Returns nil if the user has no destination.
func (s *Service) providerForDestination(ctx context.Context, userID string) Provider {
	var name string
	if err := s.db.QueryRowContext(ctx,
		`SELECT provider FROM calendar_connections WHERE user_id = ? AND is_destination = 1 LIMIT 1`, userID).Scan(&name); err != nil {
		return nil
	}
	return s.providers[name]
}

// Connected reports whether the user has any calendar connection, and which provider.
func (s *Service) Connected(ctx context.Context, userID string) (bool, string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT provider FROM calendar_connections WHERE user_id = ? ORDER BY is_destination DESC, created_at ASC LIMIT 1`, userID).Scan(&name)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, name, nil
}

// CanAutoGenerate reports whether the user's connected calendar can natively
// auto-generate the meeting link for an online location type: Google Meet from any
// connected Google calendar, Microsoft Teams only from a connected work/school
// Microsoft account (personal Microsoft accounts can't mint Teams-for-Business
// links). An account_kind of "" (unknown — legacy rows) is treated as capable.
// Returns false when the user has no connection or the type isn't an online type.
func (s *Service) CanAutoGenerate(ctx context.Context, userID, locType string) (bool, error) {
	var provider, kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT provider, COALESCE(account_kind, '') FROM calendar_connections WHERE user_id = ? AND is_destination = 1 LIMIT 1`,
		userID).Scan(&provider, &kind)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch locType {
	case "google_meet":
		return provider == "google", nil
	case "teams":
		return provider == "microsoft" && kind != "personal", nil
	default:
		return false, nil
	}
}

// Disconnect removes all calendar connections for the user (any provider), including their
// per-account calendar selections.
//
// connection_calendars has no foreign key to calendar_connections on purpose - the
// connection row is deleted and re-inserted under a new id on every token refresh, so an FK
// would cascade a user's selections away hourly (see migration 00049). The cost of that
// decision is that disconnect flows must clear the rows themselves, which is exactly what
// was missing: the migration's own comment claimed they did, and nothing did.
func (s *Service) Disconnect(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_connections WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connection_calendars WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// Connection is one connected calendar account for the multi-calendar UI/API.
type Connection struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	AccountEmail   string `json:"account_email"`
	IsDestination  bool   `json:"is_destination"`
	CheckConflicts bool   `json:"check_conflicts"`
}

// Connections lists the user's connected calendars (all providers), destination first.
func (s *Service) Connections(ctx context.Context, userID string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider, COALESCE(account_email,''), is_destination, check_conflicts
		FROM calendar_connections WHERE user_id = ?
		ORDER BY is_destination DESC, created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		var c Connection
		var dest, check int
		if err := rows.Scan(&c.ID, &c.Provider, &c.AccountEmail, &dest, &check); err != nil {
			return nil, err
		}
		c.IsDestination = dest != 0
		c.CheckConflicts = check != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetDestination makes one connected ACCOUNT the user's single write destination (clearing
// the flag on the rest).
//
// Keyed on (provider, accountEmail) rather than the connection row id: that id is recreated
// on every OAuth token refresh, and listing an account's calendars can itself trigger one.
// A page that had loaded before the refresh then held a dead id, and choosing a destination
// failed with "calendar connection not found" for no reason the user could see. The
// calendars endpoints were already hardened this way; this one was not.
func (s *Service) SetDestination(ctx context.Context, userID, provider, accountEmail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var owned int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendar_connections
		 WHERE user_id = ? AND provider = ? AND COALESCE(account_email,'') = ?`,
		userID, provider, accountEmail).Scan(&owned); err != nil {
		return err
	}
	if owned == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE calendar_connections SET is_destination = 0 WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE calendar_connections SET is_destination = 1
		 WHERE user_id = ? AND provider = ? AND COALESCE(account_email,'') = ?`,
		userID, provider, accountEmail); err != nil {
		return err
	}
	// Choosing the account clears any sub-calendar pick inside a DIFFERENT account, which
	// would otherwise still claim to be the write target in the picker while the write path
	// resolved this account instead.
	if _, err := tx.ExecContext(ctx,
		`UPDATE connection_calendars SET is_destination = 0
		 WHERE user_id = ? AND NOT (provider = ? AND account_email = ?)`,
		userID, provider, accountEmail); err != nil {
		return err
	}
	return tx.Commit()
}

// DisconnectOne removes ONE connected account and its calendar selections. If it was the
// write destination, the oldest remaining connection is promoted so the user keeps a target.
//
// Keyed on (provider, accountEmail), not the connection row id, for the same reason as
// SetDestination: the id is recreated on every token refresh. The previous version was
// worse than a wrong lookup - a stale id hit "no rows" and returned nil, so the API replied
// 204 Success having deleted nothing, and the account simply stayed on the page with no
// error to explain it. A missing account is now reported, not swallowed.
func (s *Service) DisconnectOne(ctx context.Context, userID, provider, accountEmail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var wasDest int
	switch err := tx.QueryRowContext(ctx,
		`SELECT is_destination FROM calendar_connections
		 WHERE user_id = ? AND provider = ? AND COALESCE(account_email,'') = ?`,
		userID, provider, accountEmail).Scan(&wasDest); err {
	case nil:
	case sql.ErrNoRows:
		return sql.ErrNoRows
	default:
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM calendar_connections
		 WHERE user_id = ? AND provider = ? AND COALESCE(account_email,'') = ?`,
		userID, provider, accountEmail); err != nil {
		return err
	}
	// Without this the account's calendar picks survive the disconnect, and reconnecting the
	// same address silently inherits them - including a destination pointing at a calendar
	// the user may no longer have. COALESCE matches the lookup above: a legacy row with a
	// NULL account_email would otherwise keep its calendars while losing its connection.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM connection_calendars
		 WHERE user_id = ? AND provider = ? AND COALESCE(account_email,'') = ?`,
		userID, provider, accountEmail); err != nil {
		return err
	}

	if wasDest != 0 {
		var nextID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM calendar_connections WHERE user_id = ? ORDER BY created_at ASC LIMIT 1`, userID).Scan(&nextID); err == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE calendar_connections SET is_destination = 1 WHERE id = ?`, nextID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// HasDestination reports whether the user has a destination calendar to write events to.
// Used to gate the email .ics fallback and skip pointless retries.
func (s *Service) HasDestination(ctx context.Context, userID string) (bool, error) {
	if p := s.providerForDestination(ctx, userID); p != nil {
		return p.HasDestination(ctx, userID)
	}
	return false, nil
}

// InvitesGuests reports whether the user's DESTINATION provider emails guests itself.
func (s *Service) InvitesGuests(ctx context.Context, userID string) bool {
	if p := s.providerForDestination(ctx, userID); p != nil {
		return p.InvitesGuests()
	}
	return false
}

func (s *Service) FreeBusy(ctx context.Context, userID string, from, to time.Time) ([]slots.Interval, error) {
	var out []slots.Interval
	for _, p := range s.providers {
		iv, err := p.FreeBusy(ctx, userID, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, iv...)
	}
	return out, nil
}

// ListEvents reads one user's events in [from, to) from every provider they have a
// connection with, stamping each event's Source with the provider that returned it.
//
// Routed by the providers stored on calendar_connections rather than by asking every
// registered provider, and the provider list is materialised before any network call:
// the pool is a single SQLite connection (ARCHITECTURE §17), and a provider's ListEvents
// runs its own queries, which would deadlock against an open cursor here.
//
// Unlike FreeBusy, which must fail closed (a missed busy block is a double booking), this
// is a read-only view: one provider's failure does not discard what the others returned.
// The events collected so far are returned together with the first error, and the caller
// decides whether to show the partial result.
func (s *Service) ListEvents(ctx context.Context, userID string, from, to time.Time) ([]ExternalEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT provider FROM calendar_connections WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error; nothing more actionable
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close() // #nosec G104 -- rows already fully consumed above; nothing actionable on close error
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ExternalEvent
	var firstErr error
	for _, n := range names {
		p := s.providers[n]
		if p == nil {
			continue // connection to a provider that is no longer configured
		}
		evs, err := p.ListEvents(ctx, userID, from, to)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for i := range evs {
			evs[i].Source = n
		}
		out = append(out, evs...)
	}
	return out, firstErr
}

// ConnectedUserIDs returns the ids of the users who have at least one calendar
// connection, so a caller fanning out per user can skip the ones with nothing to read.
func (s *Service) ConnectedUserIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT user_id FROM calendar_connections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// CreateEvent creates an event on the user's DESTINATION calendar. Returns the event id,
// the join URL, the calendar it was written to, and the provider that wrote it;
// ("","","","",nil) if they have no destination. Persist the calendar id AND the provider
// alongside the event id: the provider stamp is what routes later updates and cancels
// after a destination move (issue #58).
func (s *Service) CreateEvent(ctx context.Context, userID string, p CreateEventParams) (string, string, string, string, error) {
	if pr := s.providerForDestination(ctx, userID); pr != nil {
		eventID, joinURL, calendarID, err := pr.CreateEvent(ctx, userID, p)
		if err != nil {
			return "", "", "", "", err
		}
		return eventID, joinURL, calendarID, pr.Name(), nil
	}
	return "", "", "", "", nil
}

// providerForEvent resolves the provider an existing event belongs to: the stamped
// provider recorded at creation (empty for rows written before stamping), else one
// that recognizes its id (EventRecognizer), else the user's destination provider.
// Returns nil if none. A stamped provider that is no longer registered falls through
// to recognition and destination rather than stranding the event.
func (s *Service) providerForEvent(ctx context.Context, userID, eventID, storedProvider string) Provider {
	if storedProvider != "" {
		if pr := s.providers[storedProvider]; pr != nil {
			return pr
		}
	}
	for _, name := range s.ProviderNames() {
		if r, ok := s.providers[name].(EventRecognizer); ok && r.RecognizesEvent(eventID) {
			return s.providers[name]
		}
	}
	return s.providerForDestination(ctx, userID)
}

// UpdateEvent moves an event. calendarID is the one recorded at creation; provider is
// the stamped provider recorded alongside it ("" for pre-stamp rows: recognition, then
// the user's current destination).
func (s *Service) UpdateEvent(ctx context.Context, userID, calendarID, eventID, provider string, start, end time.Time, location string) error {
	if pr := s.providerForEvent(ctx, userID, eventID, provider); pr != nil {
		return pr.UpdateEvent(ctx, userID, calendarID, eventID, start, end, location)
	}
	return nil
}

// DescriptionSetter is implemented by providers that can rewrite an event's
// description WITHOUT notifying its guests. It exists for one job: taking the
// reschedule and cancel links off a booking's event just before it is deleted, because
// Google's cancellation email quotes the event's description at that moment, and links
// to manage a meeting that has just been cancelled only confuse the guest.
//
// Optional: Microsoft Graph sends attendees an update for an organizer's edit, which
// would be a second, noisier email, and CalDAV invites are best-effort anyway.
type DescriptionSetter interface {
	SetDescription(ctx context.Context, userID, calendarID, eventID, plain, rich string) error
}

// SetDescription rewrites an event's description silently, when the provider that owns
// the event supports it (DescriptionSetter); otherwise it does nothing.
func (s *Service) SetDescription(ctx context.Context, userID, calendarID, eventID, provider, plain, rich string) error {
	if eventID == "" {
		return nil
	}
	if ds, ok := s.providerForEvent(ctx, userID, eventID, provider).(DescriptionSetter); ok {
		return ds.SetDescription(ctx, userID, calendarID, eventID, plain, rich)
	}
	return nil
}

// CancelEvent deletes an event. calendarID and provider are the ones recorded at
// creation; provider "" falls back as for UpdateEvent.
func (s *Service) CancelEvent(ctx context.Context, userID, calendarID, eventID, provider string) error {
	if pr := s.providerForEvent(ctx, userID, eventID, provider); pr != nil {
		return pr.CancelEvent(ctx, userID, calendarID, eventID)
	}
	return nil
}
