package webhook

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/bookingsnap"
	"github.com/calnode/calnode/internal/uid"
)

var ErrNotFound = errors.New("webhook: not found")

type Webhook struct {
	ID     string
	UserID string
	URL    string
	Events []string
	Fields []string // payload field keys; nil means the default set
	// Scope is ScopeUser (fires for bookings UserID hosts) or ScopeOrg (fires for every
	// booking in the workspace; created and managed by admins).
	Scope string
	// EventTypes limits the webhook to bookings of these event type slugs; empty is all.
	EventTypes []string
	// PreviousSecretUntil is set while a rotated-out secret still signs deliveries.
	PreviousSecretUntil *time.Time
	IsActive            bool
	CreatedAt           time.Time
}

// Payload field keys (the JSON keys in a delivery's "data" object).
const (
	FieldID              = "id"
	FieldStatus          = "status"
	FieldStartAt         = "start_at"
	FieldEndAt           = "end_at"
	FieldCreatedAt       = "created_at"
	FieldLocation        = "location_value"
	FieldCancelReason    = "cancellation_reason"
	FieldPreviousStartAt = "previous_start_at"
	FieldPreviousEndAt   = "previous_end_at"
	FieldEventTypeSlug   = "event_type_slug"
	FieldEventTypeName   = "event_type_name"
	FieldHostID          = "host_id"
	FieldHostName        = "host_name"
	FieldHostEmail       = "host_email"
	FieldAttendeeName    = "attendee_name"
	FieldAttendeeEmail   = "attendee_email"
	FieldAttendeeTZ      = "attendee_timezone"
	FieldAnswers         = "answers"
	FieldPaymentStatus   = "payment_status"
	FieldAmountPaid      = "amount_paid_cents"
	FieldCurrency        = "amount_paid_currency"
	FieldRSVPStatus      = "rsvp_status"
	// FieldInitiatedBy says who caused a change: InitiatedByBooker (through the booking's
	// manage link, from the confirmation email or the calendar invite), InitiatedByHost (a
	// host of the booking, signed in or by API key), InitiatedByAdmin (an admin who does
	// not host it) or InitiatedBySystem (Calnode itself, e.g. the calendar sweep).
	FieldInitiatedBy = "initiated_by"

	// The full-shape fields (docs/webhooks.md). meeting, revision, occurred_at, admin_url
	// and changed carry no personal data and are in the default set; hosts and attendees do.
	FieldMeeting           = "meeting"
	FieldHosts             = "hosts"
	FieldAttendees         = "attendees"
	FieldRevision          = "revision"
	FieldOccurredAt        = "occurred_at"
	FieldAdminURL          = "admin_url"
	FieldChanged           = "changed"
	FieldPreviousHostID    = "previous_host_id"
	FieldPreviousHostName  = "previous_host_name"
	FieldPreviousHostEmail = "previous_host_email"
)

// Booking events.
const (
	EventBookingCreated     = "booking.created"
	EventBookingRescheduled = "booking.rescheduled"
	EventBookingCancelled   = "booking.cancelled"
	EventBookingReassigned  = "booking.reassigned"
	EventBookingUpdated     = "booking.updated"
	EventBookingRSVP        = "booking.rsvp"
)

// AllFields is every selectable field, in payload order. Used to validate config
// and (UI) to render the checkbox list.
var AllFields = []string{
	FieldID, FieldStatus, FieldStartAt, FieldEndAt, FieldCreatedAt,
	FieldLocation, FieldCancelReason, FieldPreviousStartAt, FieldPreviousEndAt,
	FieldEventTypeSlug, FieldEventTypeName,
	FieldHostID, FieldHostName, FieldHostEmail,
	FieldAttendeeName, FieldAttendeeEmail, FieldAttendeeTZ, FieldAnswers,
	FieldPaymentStatus, FieldAmountPaid, FieldCurrency, FieldRSVPStatus, FieldInitiatedBy,
	FieldMeeting, FieldHosts, FieldAttendees, FieldRevision, FieldOccurredAt, FieldAdminURL,
	FieldChanged, FieldPreviousHostID, FieldPreviousHostName, FieldPreviousHostEmail,
}

// Webhook scopes.
const (
	ScopeUser = "user"
	ScopeOrg  = "org"
)

// InitiatedBy values (FieldInitiatedBy).
const (
	InitiatedByBooker = "booker"
	InitiatedByHost   = "host"
	InitiatedByAdmin  = "admin"
	InitiatedBySystem = "system"
)

// defaultFields reproduces the original payload (no PII, no answers) so a webhook with no
// field config (fields IS NULL) keeps its historical shape. Payment fields are included but
// omitempty, so free bookings are byte-identical to before; only paid bookings gain them.
var defaultFields = []string{
	FieldID, FieldEventTypeSlug, FieldHostID, FieldStartAt, FieldEndAt,
	FieldStatus, FieldLocation, FieldCancelReason, FieldCreatedAt,
	FieldPreviousStartAt, FieldPreviousEndAt,
	FieldPaymentStatus, FieldAmountPaid, FieldCurrency, FieldRSVPStatus, FieldInitiatedBy,
	FieldMeeting, FieldRevision, FieldOccurredAt, FieldAdminURL, FieldChanged, FieldPreviousHostID,
}

var validField = func() map[string]bool {
	m := make(map[string]bool, len(AllFields))
	for _, f := range AllFields {
		m[f] = true
	}
	return m
}()

// ValidFields filters fields to known keys, preserving order. Used to sanitise
// caller-supplied config.
func ValidFields(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if validField[f] {
			out = append(out, f)
		}
	}
	return out
}

type Delivery struct {
	ID              string
	WebhookID       string
	BookingID       string
	Event           string
	Status          string
	ResponseStatus  *int
	AttemptCount    int
	LastAttemptedAt *string
}

// BookingPayload is the "data" object inside every webhook envelope.
type BookingPayload struct {
	ID                 string `json:"id"`
	EventTypeSlug      string `json:"event_type_slug"`
	HostID             string `json:"host_id"`
	StartAt            string `json:"start_at"`
	EndAt              string `json:"end_at"`
	Status             string `json:"status"`
	CancellationReason string `json:"cancellation_reason,omitempty"`
	LocationValue      string `json:"location_value,omitempty"`
	CreatedAt          string `json:"created_at"`
	PreviousStartAt    string `json:"previous_start_at,omitempty"`
	PreviousEndAt      string `json:"previous_end_at,omitempty"`
	PaymentStatus      string `json:"payment_status,omitempty"`
	AmountPaidCents    int    `json:"amount_paid_cents,omitempty"`
	AmountPaidCurrency string `json:"amount_paid_currency,omitempty"`
	// RSVPStatus is the booker's answer to a Calnode-sent invite (accepted, declined,
	// tentative); set on booking.rsvp only.
	RSVPStatus string `json:"rsvp_status,omitempty"`
	// InitiatedBy is who caused a booking.cancelled or booking.rescheduled
	// (InitiatedByBooker / InitiatedByHost); empty on other events.
	InitiatedBy string `json:"initiated_by,omitempty"`
	// Changed names the fields a booking.updated changed (e.g. "location_value").
	Changed []string `json:"changed,omitempty"`
	// PreviousHostID is the host a booking.reassigned moved the booking away from.
	PreviousHostID string `json:"previous_host_id,omitempty"`
	// OccurredAt is when the change happened (RFC3339). Empty: the booking's changed_at.
	OccurredAt string `json:"occurred_at,omitempty"`
}

type Service struct {
	db      *sql.DB
	key     [32]byte
	baseURL string // public URL, for admin_url
}

// SetBaseURL sets the instance's public URL, used to build admin_url in payloads.
func (s *Service) SetBaseURL(u string) { s.baseURL = u }

// New creates a Service. If encKeyHex is empty an ephemeral key is generated
// (secrets won't survive restarts but the server still works in dev/test).
func New(db *sql.DB, encKeyHex string) (*Service, error) {
	s := &Service{db: db}
	if encKeyHex != "" {
		b, err := hex.DecodeString(encKeyHex)
		if err != nil || len(b) != 32 {
			return nil, fmt.Errorf("webhook: encryption key must be 64 hex chars")
		}
		copy(s.key[:], b)
	} else {
		if _, err := io.ReadFull(rand.Reader, s.key[:]); err != nil {
			return nil, fmt.Errorf("webhook: generate ephemeral key: %w", err)
		}
	}
	return s, nil
}

// Create registers a new webhook and returns the Webhook plus the plain-text
// signing secret (shown only once; stored encrypted).
// Create registers a webhook (fields default to the unset/original-payload set;
// callers set field selection via Update).
func (s *Service) Create(ctx context.Context, userID, url string, events []string) (*Webhook, string, error) {
	rawSecret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, rawSecret); err != nil {
		return nil, "", fmt.Errorf("webhook: generate secret: %w", err)
	}
	plainSecret := hex.EncodeToString(rawSecret)

	encSecret, err := s.encrypt(rawSecret)
	if err != nil {
		return nil, "", fmt.Errorf("webhook: encrypt secret: %w", err)
	}

	eventsJSON, err := json.Marshal(events)
	if err != nil {
		return nil, "", fmt.Errorf("webhook: marshal events: %w", err)
	}

	id := uid.New()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO webhooks (id, user_id, url, events, secret_enc)
		VALUES (?, ?, ?, ?, ?)`,
		id, userID, url, string(eventsJSON), encSecret)
	if err != nil {
		return nil, "", fmt.Errorf("webhook: insert: %w", err)
	}

	wh := &Webhook{
		ID:        id,
		Scope:     ScopeUser,
		UserID:    userID,
		URL:       url,
		Events:    events,
		IsActive:  true,
		CreatedAt: time.Now().UTC(),
	}
	return wh, plainSecret, nil
}

// Update applies partial changes to a webhook owned by userID. Nil pointers are
// left unchanged. Returns ErrNotFound if no such webhook exists for the user.
func (s *Service) Update(ctx context.Context, userID, id string, events, fields *[]string) error {
	var set []string
	var args []any
	if events != nil {
		eb, _ := json.Marshal(*events)
		set = append(set, "events = ?")
		args = append(args, string(eb))
	}
	if fields != nil {
		fb, _ := json.Marshal(ValidFields(*fields))
		set = append(set, "fields = ?")
		args = append(args, string(fb))
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, id, userID)
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET `+strings.Join(set, ", ")+` WHERE id = ? AND user_id = ?`, args...) // #nosec G202 -- set is built above from hardcoded "col = ?" literals only; every value is bound via args...
	if err != nil {
		return fmt.Errorf("webhook: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns all webhooks for a user, most recent first.
func (s *Service) List(ctx context.Context, userID string) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, url, events, fields, scope, event_types, secret_prev_expires_at, is_active, created_at
		FROM webhooks WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("webhook: list: %w", err)
	}
	defer rows.Close()

	var out []Webhook
	for rows.Next() {
		var wh Webhook
		var eventsJSON string
		var fieldsJSON sql.NullString
		var isActive int
		var createdAt string
		var typesJSON sql.NullString
		var prevUntil string
		if err := rows.Scan(&wh.ID, &wh.UserID, &wh.URL, &eventsJSON, &fieldsJSON, &wh.Scope, &typesJSON, &prevUntil, &isActive, &createdAt); err != nil {
			return nil, fmt.Errorf("webhook: scan: %w", err)
		}
		wh.EventTypes = []string{}
		if typesJSON.Valid && typesJSON.String != "" {
			_ = json.Unmarshal([]byte(typesJSON.String), &wh.EventTypes)
		}
		if t, err := time.Parse(time.RFC3339, prevUntil); err == nil && t.After(time.Now()) {
			wh.PreviousSecretUntil = &t
		}
		wh.IsActive = isActive == 1
		_ = json.Unmarshal([]byte(eventsJSON), &wh.Events)
		if fieldsJSON.Valid && fieldsJSON.String != "" {
			_ = json.Unmarshal([]byte(fieldsJSON.String), &wh.Fields)
		} else {
			wh.Fields = defaultFields // surface the effective set for unconfigured webhooks
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			wh.CreatedAt = t
		}
		out = append(out, wh)
	}
	return out, rows.Err()
}

// SetScope sets the scope of a webhook owned by userID (ScopeUser or ScopeOrg). The
// caller decides who may choose ScopeOrg; the service only stores it.
func (s *Service) SetScope(ctx context.Context, userID, id, scope string) error {
	if scope != ScopeUser && scope != ScopeOrg {
		return fmt.Errorf("webhook: invalid scope %q", scope)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET scope = ? WHERE id = ? AND user_id = ?`, scope, id, userID)
	if err != nil {
		return fmt.Errorf("webhook: set scope: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// OrgOwner returns the creator of webhook id when it is organisation-scoped. An admin
// manages any org webhook, so the handler acts on it as its owner.
func (s *Service) OrgOwner(ctx context.Context, id string) (string, bool) {
	var owner string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM webhooks WHERE id = ? AND scope = 'org'`, id).Scan(&owner)
	return owner, err == nil
}

// ListOrg returns every organisation-scoped webhook, most recent first.
func (s *Service) ListOrg(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT user_id FROM webhooks WHERE scope = 'org'`)
	if err != nil {
		return nil, fmt.Errorf("webhook: list org owners: %w", err)
	}
	var owners []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error
			return nil, fmt.Errorf("webhook: scan org owner: %w", err)
		}
		owners = append(owners, o)
	}
	rows.Close() // #nosec G104 -- drained; the next query needs the single connection
	var out []Webhook
	for _, o := range owners {
		list, err := s.List(ctx, o)
		if err != nil {
			return nil, err
		}
		for _, wh := range list {
			if wh.Scope == ScopeOrg {
				out = append(out, wh)
			}
		}
	}
	slices.SortFunc(out, func(a, b Webhook) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// SetEventTypes limits a webhook owned by userID to bookings of these event type slugs
// (deduplicated, blanks dropped); an empty list means every event type.
func (s *Service) SetEventTypes(ctx context.Context, userID, id string, slugs []string) error {
	var clean []string
	for _, v := range slugs {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(clean, v) {
			clean = append(clean, v)
		}
	}
	var arg any // NULL = all
	if len(clean) > 0 {
		b, _ := json.Marshal(clean)
		arg = string(b)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE webhooks SET event_types = ? WHERE id = ? AND user_id = ?`, arg, id, userID)
	if err != nil {
		return fmt.Errorf("webhook: set event types: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SecretOverlap is how long a rotated-out secret keeps signing deliveries next to the
// new one, so a receiver can switch secrets without rejecting anything in between.
const SecretOverlap = 24 * time.Hour

// RotateSecret gives a webhook owned by userID a new signing secret and returns it (hex,
// shown once). The old secret signs alongside it for SecretOverlap; deliveries in that
// window carry both signatures. Rotating again inside the window drops the oldest.
func (s *Service) RotateSecret(ctx context.Context, userID, id string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", time.Time{}, fmt.Errorf("webhook: generate secret: %w", err)
	}
	enc, err := s.encrypt(raw)
	if err != nil {
		return "", time.Time{}, err
	}
	until := time.Now().UTC().Add(SecretOverlap).Truncate(time.Second)
	res, err := s.db.ExecContext(ctx, `
		UPDATE webhooks SET secret_prev_enc = secret_enc, secret_prev_expires_at = ?, secret_enc = ?
		WHERE id = ? AND user_id = ?`, until.Format(time.RFC3339), enc, id, userID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("webhook: rotate secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", time.Time{}, ErrNotFound
	}
	return hex.EncodeToString(raw), until, nil
}

// SignatureHeader is the X-Calnode-Signature value for payload: "sha256=<hex>" with the
// current secret, followed by ",sha256=<hex>" with the previous one while it is inside
// its overlap window. The HMAC key is the secret's raw 32 bytes (hex-decoded), not the
// hex string shown to the user.
func (s *Service) SignatureHeader(secretEnc, prevEnc, prevUntil string, payload []byte, now time.Time) (string, error) {
	secret, err := s.decrypt(secretEnc)
	if err != nil {
		return "", err
	}
	sig := Sign(secret, payload)
	if prevEnc != "" {
		if t, err := time.Parse(time.RFC3339, prevUntil); err == nil && now.Before(t) {
			if prev, err := s.decrypt(prevEnc); err == nil {
				sig += "," + Sign(prev, payload)
			}
		}
	}
	return sig, nil
}

// Redeliver queues delivery deliveryID of webhook webhookID (owned by userID) to be
// sent again now, with a fresh set of attempts. It is the same delivery, so the
// X-Calnode-Delivery id and the payload are unchanged and a receiver that already
// stored it can recognise the repeat.
func (s *Service) Redeliver(ctx context.Context, userID, webhookID, deliveryID string) error {
	var owner string
	if err := s.db.QueryRowContext(ctx, `
		SELECT wh.user_id FROM webhook_deliveries d JOIN webhooks wh ON wh.id = d.webhook_id
		WHERE d.id = ? AND d.webhook_id = ?`, deliveryID, webhookID).Scan(&owner); err != nil || owner != userID {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("webhook: redeliver begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET status = 'pending' WHERE id = ?`, deliveryID); err != nil {
		return fmt.Errorf("webhook: redeliver reset: %w", err)
	}
	jobPayload, _ := json.Marshal(map[string]string{"webhook_delivery_id": deliveryID})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (id, type, payload, run_at, max_attempts) VALUES (?, 'webhook.deliver', ?, ?, ?)`,
		uid.New(), string(jobPayload), time.Now().UTC().Format(time.RFC3339), webhookDeliverAttempts); err != nil {
		return fmt.Errorf("webhook: redeliver job: %w", err)
	}
	return tx.Commit()
}

// Delete removes a webhook owned by userID. Returns ErrNotFound if it doesn't exist.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhooks WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("webhook: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// enrichedBooking is the full set of values available to a webhook payload,
// gathered once per Enqueue and then filtered per-webhook by its field list.
type enrichedBooking struct {
	core                                    BookingPayload
	eventTypeName                           string
	hostName, hostEmail                     string
	attendeeName, attendeeEmail, attendeeTZ string
	answers                                 []bookingsnap.Answer
	snap                                    *bookingsnap.Snapshot // nil when the booking row is gone
	previousHostName, previousHostEmail     string
}

// enrich loads the booking as it stands now (bookingsnap) and lets it override the
// caller's copy of the current-state fields: status, times, location, host, slug. One
// row read gives those and the revision together, so a payload's fields always match
// its revision, whatever order events are built in. That is also what makes
// booking.created carry the Meet/Teams link the calendar minted after the caller took
// its copy. The caller still owns what the row does not hold: previous times, the
// actor, payment, what changed. Best-effort: a booking that cannot be read keeps the
// caller's values.
func (s *Service) enrich(ctx context.Context, p BookingPayload) enrichedBooking {
	bd := enrichedBooking{core: p, answers: []bookingsnap.Answer{}}
	if p.ID == "" {
		return bd
	}
	snap, err := bookingsnap.Load(ctx, s.db, p.ID, s.baseURL)
	if err != nil {
		return bd
	}
	bd.snap = snap
	c := &bd.core
	c.Status = snap.Status
	c.StartAt = rfc3339(snap.StartAt)
	c.EndAt = rfc3339(snap.EndAt)
	c.CreatedAt = rfc3339(snap.CreatedAt)
	c.LocationValue = snap.LocationValue
	c.HostID = snap.HostID
	c.EventTypeSlug = snap.EventTypeSlug
	if c.CancellationReason == "" {
		c.CancellationReason = snap.CancellationReason
	}
	if c.OccurredAt == "" {
		c.OccurredAt = rfc3339(snap.ChangedAt)
	}
	bd.eventTypeName, bd.hostName, bd.hostEmail = snap.EventTypeName, snap.HostName, snap.HostEmail
	for _, a := range snap.Attendees {
		if a.Organizer {
			bd.attendeeName, bd.attendeeEmail, bd.attendeeTZ = a.Name, a.Email, a.Timezone
		}
	}
	bd.answers = snap.Answers
	if p.PreviousHostID != "" {
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(name,''), COALESCE(email,'') FROM users WHERE id = ?`,
			p.PreviousHostID).Scan(&bd.previousHostName, &bd.previousHostEmail)
	}
	return bd
}

// rfc3339 normalises a stored timestamp to RFC3339 in UTC, the format every payload
// time uses. A value that does not parse is returned unchanged.
func rfc3339(v string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// buildData renders the "data" object containing only the requested field keys.
// The optional booking fields are omitted when empty (matching the original
// payload's omitempty behaviour); everything else is always included when selected.
func buildData(bd enrichedBooking, fields []string) map[string]any {
	always := map[string]any{
		FieldID:            bd.core.ID,
		FieldStatus:        bd.core.Status,
		FieldStartAt:       bd.core.StartAt,
		FieldEndAt:         bd.core.EndAt,
		FieldCreatedAt:     bd.core.CreatedAt,
		FieldEventTypeSlug: bd.core.EventTypeSlug,
		FieldEventTypeName: bd.eventTypeName,
		FieldHostID:        bd.core.HostID,
		FieldHostName:      bd.hostName,
		FieldHostEmail:     bd.hostEmail,
		FieldAttendeeName:  bd.attendeeName,
		FieldAttendeeEmail: bd.attendeeEmail,
		FieldAttendeeTZ:    bd.attendeeTZ,
		FieldAnswers:       bd.answers,
	}
	if bd.snap != nil {
		always[FieldMeeting] = bd.snap.Meeting
		always[FieldHosts] = bd.snap.Hosts
		always[FieldAttendees] = bd.snap.Attendees
		always[FieldRevision] = bd.snap.Revision
	}
	omitEmpty := map[string]string{
		FieldLocation:          bd.core.LocationValue,
		FieldCancelReason:      bd.core.CancellationReason,
		FieldPreviousStartAt:   bd.core.PreviousStartAt,
		FieldPreviousEndAt:     bd.core.PreviousEndAt,
		FieldPaymentStatus:     bd.core.PaymentStatus,
		FieldCurrency:          bd.core.AmountPaidCurrency,
		FieldRSVPStatus:        bd.core.RSVPStatus,
		FieldInitiatedBy:       bd.core.InitiatedBy,
		FieldOccurredAt:        bd.core.OccurredAt,
		FieldPreviousHostID:    bd.core.PreviousHostID,
		FieldPreviousHostName:  bd.previousHostName,
		FieldPreviousHostEmail: bd.previousHostEmail,
	}
	if bd.snap != nil {
		omitEmpty[FieldAdminURL] = bd.snap.AdminURL
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := always[f]; ok {
			out[f] = v
		} else if v, ok := omitEmpty[f]; ok && v != "" {
			out[f] = v
		} else if f == FieldAmountPaid && bd.core.AmountPaidCents > 0 {
			out[f] = bd.core.AmountPaidCents
		} else if f == FieldChanged && len(bd.core.Changed) > 0 {
			out[f] = bd.core.Changed
		}
	}
	return out
}

// webhookDeliverAttempts is how many times a delivery is tried before it is marked
// failed. With the worker's webhook backoff (5 min, tripling, capped at 6 h) the last
// try is about 21 hours after the first, so a receiver can be down most of a day
// without losing an event (worker.webhookBackoff).
const webhookDeliverAttempts = 8

// Enqueue finds all active webhooks for p.HostID (and p.PreviousHostID, so a host hears
// that a booking moved away from them) or org-wide that subscribe to event,
// and creates a webhook_deliveries + jobs row pair for each. Failures are
// soft-errors (caller logs; a booking is already committed).
func (s *Service) Enqueue(ctx context.Context, event string, p BookingPayload) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, events, fields, event_types FROM webhooks
		WHERE is_active = 1 AND (user_id = ? OR user_id = ? OR scope = 'org')`, p.HostID, p.PreviousHostID)
	if err != nil {
		return fmt.Errorf("webhook: list for enqueue: %w", err)
	}

	type wrow struct {
		id         string
		fields     []string // nil = default set
		eventTypes []string // nil = every event type
	}
	var matching []wrow
	for rows.Next() {
		var id, eventsJSON string
		var fieldsJSON, typesJSON sql.NullString
		if err := rows.Scan(&id, &eventsJSON, &fieldsJSON, &typesJSON); err != nil {
			rows.Close() // #nosec G104 -- already returning the scan error; nothing more actionable
			return fmt.Errorf("webhook: scan: %w", err)
		}
		var events []string
		_ = json.Unmarshal([]byte(eventsJSON), &events)
		subscribed := false
		for _, e := range events {
			if e == event {
				subscribed = true
				break
			}
		}
		if !subscribed {
			continue
		}
		var types []string
		if typesJSON.Valid && typesJSON.String != "" {
			_ = json.Unmarshal([]byte(typesJSON.String), &types)
		}
		var fields []string
		if fieldsJSON.Valid && fieldsJSON.String != "" {
			_ = json.Unmarshal([]byte(fieldsJSON.String), &fields)
		}
		matching = append(matching, wrow{id: id, fields: fields, eventTypes: types})
	}
	rows.Close() // #nosec G104 -- rows already fully consumed above; nothing actionable on close error
	if err := rows.Err(); err != nil {
		return err
	}
	// The event-type filter needs the slug; callers that did not pass one (the notetaker
	// events) have it looked up from the booking.
	slug := p.EventTypeSlug
	if slug == "" && p.ID != "" {
		_ = s.db.QueryRowContext(ctx, `SELECT et.slug FROM bookings b JOIN event_types et ON et.id = b.event_type_id WHERE b.id = ?`, p.ID).Scan(&slug)
	}
	kept := matching[:0]
	for _, wh := range matching {
		if len(wh.eventTypes) == 0 || slices.Contains(wh.eventTypes, slug) {
			kept = append(kept, wh)
		}
	}
	matching = kept
	if len(matching) == 0 {
		return nil
	}

	// Gather all available data once; each webhook gets its own field-filtered copy.
	bd := s.enrich(ctx, p)
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// booking_id is a nullable FK; use NULL when empty so callers without a real
	// bookings row don't violate the constraint.
	var bookingIDArg interface{}
	if p.ID != "" {
		bookingIDArg = p.ID
	}

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("webhook: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, wh := range matching {
		fieldset := wh.fields
		if len(fieldset) == 0 {
			fieldset = defaultFields
		}
		envelope := map[string]any{
			"event":      event,
			"created_at": createdAt,
			"data":       buildData(bd, fieldset),
		}
		payloadBytes, err := json.Marshal(envelope)
		if err != nil {
			return fmt.Errorf("webhook: marshal payload: %w", err)
		}

		deliveryID := uid.New()
		jobID := uid.New()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO webhook_deliveries (id, webhook_id, booking_id, event, payload, status)
			VALUES (?, ?, ?, ?, ?, 'pending')`,
			deliveryID, wh.id, bookingIDArg, event, string(payloadBytes)); err != nil {
			return fmt.Errorf("webhook: insert delivery: %w", err)
		}
		jobPayload, _ := json.Marshal(map[string]string{"webhook_delivery_id": deliveryID})
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, type, payload, run_at, max_attempts)
			VALUES (?, 'webhook.deliver', ?, ?, ?)`,
			jobID, string(jobPayload), now, webhookDeliverAttempts); err != nil {
			return fmt.Errorf("webhook: insert job: %w", err)
		}
	}
	return tx.Commit()
}

// ListDeliveries returns the 50 most recent deliveries for a webhook.
func (s *Service) ListDeliveries(ctx context.Context, userID, webhookID string) ([]Delivery, error) {
	var ownerID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM webhooks WHERE id = ?`, webhookID).Scan(&ownerID); err != nil {
		return nil, ErrNotFound
	}
	if ownerID != userID {
		return nil, ErrNotFound
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(booking_id,''), event, status,
		       response_status, attempt_count, last_attempted_at
		FROM webhook_deliveries WHERE webhook_id = ?
		ORDER BY rowid DESC LIMIT 50`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("webhook: list deliveries: %w", err)
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		var d Delivery
		var respStatus sql.NullInt64
		var lastAt sql.NullString
		if err := rows.Scan(&d.ID, &d.BookingID, &d.Event, &d.Status,
			&respStatus, &d.AttemptCount, &lastAt); err != nil {
			return nil, fmt.Errorf("webhook: scan delivery: %w", err)
		}
		d.WebhookID = webhookID
		if respStatus.Valid {
			v := int(respStatus.Int64)
			d.ResponseStatus = &v
		}
		if lastAt.Valid && lastAt.String != "" {
			s := lastAt.String
			d.LastAttemptedAt = &s
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DecryptSecret decrypts the stored secret_enc for use by the worker when signing deliveries.
func (s *Service) DecryptSecret(encSecret string) ([]byte, error) {
	return s.decrypt(encSecret)
}

// Sign returns the HMAC-SHA256 signature header value for a payload.
func Sign(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", fmt.Errorf("webhook: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("webhook: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("webhook: nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Service) decrypt(encoded string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("webhook: base64: %w", err)
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, fmt.Errorf("webhook: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webhook: gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(b) < ns {
		return nil, fmt.Errorf("webhook: ciphertext too short")
	}
	plain, err := gcm.Open(nil, b[:ns], b[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("webhook: decrypt: %w", err)
	}
	return plain, nil
}
