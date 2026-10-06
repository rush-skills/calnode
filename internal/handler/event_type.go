package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/richtext"
	"github.com/calnode/calnode/internal/uid"
)

const (
	defaultMsgConfirmation = "Looking forward to our meeting! Feel free to reply if you have any questions beforehand."
	defaultMsgCancellation = "Apologies for the cancellation. You're welcome to rebook at any time."
	defaultMsgReschedule   = "Apologies for the change — looking forward to connecting at the new time!"
	defaultMsgReminder     = "Please reach out if you need to make any last-minute changes."
)

type eventTypeJSON struct {
	AllowPhoneCall      bool    `json:"allow_phone_call"`
	ID                  string  `json:"id"`
	Slug                string  `json:"slug"`
	Name                string  `json:"name"`
	Description         *string `json:"description"`
	DurationMinutes     int     `json:"duration_minutes"`
	SlotIntervalMinutes int     `json:"slot_interval_minutes"`
	LocationType        string  `json:"location_type"`
	LocationValue       *string `json:"location_value"`
	RoutingMode         string  `json:"routing_mode"`
	RRStrategy          string  `json:"rr_strategy"`
	BufferBeforeMinutes int     `json:"buffer_before_minutes"`
	BufferAfterMinutes  int     `json:"buffer_after_minutes"`
	MinNoticeMinutes    int     `json:"min_notice_minutes"`
	MaxFutureDays       int     `json:"max_future_days"`
	MaxActiveBookings   int     `json:"max_active_bookings"`
	IsActive            bool    `json:"is_active"`
	// ShowTakenSlots renders already-booked times greyed out on the booking page
	// instead of omitting them. Off by default: the slots endpoint is public, so this
	// makes the host's booked hours legible to anyone with the link (#19).
	ShowTakenSlots bool   `json:"show_taken_slots"`
	IsPublic       bool   `json:"is_public"`
	CreatedAt      string `json:"created_at"`
	// CalendarMessage is sanitized HTML placed on every booking's calendar invite
	// (above the Booking ID line) — distinct from Description, which is the public
	// booking page's text. nil/empty = the invite carries only the Booking ID.
	CalendarMessage *string `json:"calendar_message"`
	MsgConfirmation *string `json:"msg_confirmation"`
	MsgCancellation *string `json:"msg_cancellation"`
	MsgReschedule   *string `json:"msg_reschedule"`
	MsgReminder     *string `json:"msg_reminder"`
	// MsgGreeting overrides the conversational assistant's opening line for this event
	// type. Unlike the Msg* fields above, nil/empty means "no override" — the assistant
	// falls back to the locale-keyed default greeting, not a fixed English seed.
	MsgGreeting      *string `json:"msg_greeting"`
	SubjConfirmation *string `json:"subj_confirmation"`
	SubjCancellation *string `json:"subj_cancellation"`
	SubjReschedule   *string `json:"subj_reschedule"`
	SubjReminder     *string `json:"subj_reminder"`
	PriceCents       int     `json:"price_cents"` // 0 = free
	Currency         string  `json:"currency"`    // ISO 4217, lowercase (e.g. "usd")
	Reminders        []int   `json:"reminders"`   // hours_before values
	// Archived is true when the event type has been archived — hidden from the default
	// list, with is_active forced off so it stops taking bookings. Reversible.
	Archived bool `json:"archived"`
	// Visibility is "org" (every member sees it, admins may edit) or "private" (owner
	// only, plus assigned hosts read-only). See event_type_access.go.
	Visibility string `json:"visibility"`
	// OwnerID is the creator (event_types.user_id): the account whose calendar and
	// connections drive location defaults, and who is seeded as the first host.
	OwnerID string `json:"owner_id"`
	// Owned is true when the requesting user owns this event type.
	Owned bool `json:"owned"`
	// CanEdit is true when the requesting user may change it: the owner, or an admin
	// on an org-wide event type. The UI renders read-only when false; the server
	// enforces the same rule on every write (eventTypeIDForEditor).
	CanEdit bool `json:"can_edit"`
	// OwnerName/OwnerEmail identify the owner so a read-only viewer knows who created
	// it and who to contact for changes. Populated by list and get.
	OwnerName  string `json:"owner_name,omitempty"`
	OwnerEmail string `json:"owner_email,omitempty"`
}

// stampViewer fills the per-viewer fields (Owned, CanEdit) from the row's owner and
// visibility. Every response that carries an event type goes through this so the two
// flags can never disagree with the write-side rule.
func (et *eventTypeJSON) stampViewer(user AuthUser) {
	et.Owned = et.OwnerID == user.ID
	et.CanEdit = canEditEventType(user, et.OwnerID, et.Visibility)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEventType(s rowScanner) (*eventTypeJSON, error) {
	return scanEventTypeRow(s)
}

// scanEventTypeRow scans the base event-type columns, plus any `trailing` dest
// pointers (e.g. the computed `owned` column from the list/get queries).
func scanEventTypeRow(s rowScanner, trailing ...any) (*eventTypeJSON, error) {
	var et eventTypeJSON
	var desc, locVal, msgConf, msgCancel, msgResched, msgRemind, msgGreeting sql.NullString
	var subjConf, subjCancel, subjResched, subjRemind, calMsg sql.NullString
	var isActive, isPublic, showTaken int

	dests := []any{
		&et.ID, &et.Slug, &et.Name, &desc,
		&et.DurationMinutes, &et.SlotIntervalMinutes,
		&et.LocationType, &locVal, &et.AllowPhoneCall,
		&et.RoutingMode, &et.RRStrategy,
		&et.BufferBeforeMinutes, &et.BufferAfterMinutes,
		&et.MinNoticeMinutes, &et.MaxFutureDays, &et.MaxActiveBookings,
		&isActive, &isPublic, &showTaken, &et.CreatedAt,
		&msgConf, &msgCancel, &msgResched, &msgRemind, &msgGreeting,
		&subjConf, &subjCancel, &subjResched, &subjRemind,
		&et.PriceCents, &et.Currency, &calMsg,
		&et.OwnerID, &et.Visibility,
	}
	dests = append(dests, trailing...)
	err := s.Scan(dests...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if desc.Valid {
		et.Description = &desc.String
	}
	if locVal.Valid {
		et.LocationValue = &locVal.String
	}
	et.IsActive = isActive != 0
	et.IsPublic = isPublic != 0
	et.ShowTakenSlots = showTaken != 0
	if calMsg.Valid && calMsg.String != "" {
		et.CalendarMessage = &calMsg.String
	}
	if msgConf.Valid {
		et.MsgConfirmation = &msgConf.String
	}
	if msgCancel.Valid {
		et.MsgCancellation = &msgCancel.String
	}
	if msgResched.Valid {
		et.MsgReschedule = &msgResched.String
	}
	if msgRemind.Valid {
		et.MsgReminder = &msgRemind.String
	}
	if msgGreeting.Valid {
		et.MsgGreeting = &msgGreeting.String
	}
	if subjConf.Valid {
		et.SubjConfirmation = &subjConf.String
	}
	if subjCancel.Valid {
		et.SubjCancellation = &subjCancel.String
	}
	if subjResched.Valid {
		et.SubjReschedule = &subjResched.String
	}
	if subjRemind.Valid {
		et.SubjReminder = &subjRemind.String
	}
	et.Reminders = []int{} // initialise to non-nil so JSON encodes as [] not null
	return &et, nil
}

const etColumns = `id, slug, name, description,
	duration_minutes, slot_interval_minutes,
	location_type, location_value, allow_phone_call,
	routing_mode, rr_strategy,
	buffer_before_minutes, buffer_after_minutes,
	min_notice_minutes, max_future_days, max_active_bookings,
	is_active, is_public, show_taken_slots, created_at,
	msg_confirmation, msg_cancellation, msg_reschedule, msg_reminder, msg_greeting,
	subj_confirmation, subj_cancellation, subj_reschedule, subj_reminder,
	price_cents, currency, calendar_message,
	user_id, visibility`

// selectETCols fetches a single event type by whatever WHERE the caller appends.
// Access is decided before this runs (eventTypeIDForEditor/Viewer), so it is unscoped.
const selectETCols = "SELECT " + etColumns + " FROM event_types"

// etOwnerColumns adds the owner's name and email to etColumns, so a viewer who is
// not the owner knows who created the event type and who to ask for changes.
const etOwnerColumns = etColumns + `,
	(SELECT name FROM users WHERE id = event_types.user_id) AS owner_name,
	(SELECT email FROM users WHERE id = event_types.user_id) AS owner_email,
	(archived_at IS NOT NULL) AS archived`

// listEventTypesQuery returns every event type the member may see: all org-wide
// ones, their own, and any they are assigned to host (eventTypeVisibleFilter). Owned
// and can_edit are derived in Go from user_id + visibility (stampViewer); ownership is
// keyed on event_types.user_id, not host membership — the owner is also seeded into
// event_type_hosts. Binds: viewer id ×2.
const listEventTypesQuery = "SELECT " + etOwnerColumns + `
FROM event_types
WHERE ` + eventTypeVisibleFilter + `
ORDER BY created_at`

// getEventTypeQuery fetches one event type by slug under the same visibility rule as
// the list. Binds: slug, viewer id ×2.
const getEventTypeQuery = "SELECT " + etOwnerColumns + `
FROM event_types
WHERE slug = ? AND ` + eventTypeVisibleFilter

// loadReminders fetches the hours_before list for an event type and sets et.Reminders.
func (h *Handler) loadReminders(ctx context.Context, etID string, et *eventTypeJSON) error {
	rows, err := h.db.QueryContext(ctx,
		`SELECT hours_before FROM event_type_reminders WHERE event_type_id = ? ORDER BY hours_before DESC`, etID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hb int
		if err := rows.Scan(&hb); err != nil {
			return err
		}
		et.Reminders = append(et.Reminders, hb)
	}
	return rows.Err()
}

// validReminderHours is the allowed set of hours_before values.
var validReminderHours = map[int]bool{1: true, 2: true, 4: true, 8: true, 12: true, 24: true, 48: true, 72: true, 168: true}

// CreateEventType handles POST /v1/event-types.
func (h *Handler) CreateEventType(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		Slug                string  `json:"slug"`
		Name                string  `json:"name"`
		Description         *string `json:"description"`
		DurationMinutes     int     `json:"duration_minutes"`
		SlotIntervalMinutes *int    `json:"slot_interval_minutes"`
		LocationType        *string `json:"location_type"`
		LocationValue       *string `json:"location_value"`
		RoutingMode         *string `json:"routing_mode"`
		BufferBeforeMinutes *int    `json:"buffer_before_minutes"`
		BufferAfterMinutes  *int    `json:"buffer_after_minutes"`
		MinNoticeMinutes    *int    `json:"min_notice_minutes"`
		MaxFutureDays       *int    `json:"max_future_days"`
		MaxActiveBookings   *int    `json:"max_active_bookings"`
		AllowPhoneCall      *bool   `json:"allow_phone_call"`
		ShowTakenSlots      *bool   `json:"show_taken_slots"`
		Visibility          *string `json:"visibility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// The slug is the public booking URL, so it is always normalised (slugify is the
	// only rule: lowercase, letters and digits, hyphens between). It used to be stored
	// as typed, and "Intro Call" made a /book/ link that never resolved. An omitted
	// slug derives from the name, as teams do; one that normalises to nothing is a 400
	// rather than a silent fallback, because the caller did say what they wanted.
	slug := slugify(req.Slug)
	slugOmitted := strings.TrimSpace(req.Slug) == ""
	if slugOmitted {
		slug = slugify(req.Name)
	}
	if slug == "" {
		if slugOmitted {
			// Name the field the caller actually sent: a non-Latin name ("会議") yields
			// nothing, and "slug must contain…" would point at a field they never filled.
			h.writeError(w, http.StatusBadRequest,
				"could not derive a booking link from the name; provide a slug with letters or numbers")
			return
		}
		h.writeError(w, http.StatusBadRequest, "slug must contain letters or numbers")
		return
	}
	// Org-wide unless the creator says otherwise: a shared workspace should see what
	// exists in it. The creator is still the owner (user_id) for hosting and location
	// defaults either way.
	visibility := eventTypeVisibilityOrg
	if req.Visibility != nil {
		if !validEventTypeVisibility(*req.Visibility) {
			h.writeError(w, http.StatusBadRequest, "visibility must be 'org' or 'private'")
			return
		}
		visibility = *req.Visibility
	}
	if req.MaxActiveBookings != nil && *req.MaxActiveBookings < 0 {
		h.writeError(w, http.StatusBadRequest, "max_active_bookings cannot be negative (0 = unlimited)")
		return
	}
	if req.DurationMinutes <= 0 {
		h.writeError(w, http.StatusBadRequest, "duration_minutes must be positive")
		return
	}

	// Default the slot interval to the meeting length rather than a fixed 30. The two are
	// genuinely independent - interval is how often a slot STARTS, duration is how long it
	// runs - and keeping them separate is deliberate: a 45-minute meeting offered on the
	// hour is a reasonable thing to want. But a fixed default of 30 is the wrong guess in
	// both directions: a 15-minute event offered every 30 minutes wastes half the host's
	// day, and a 90-minute one offers starts that mostly cannot be honoured. Matching the
	// duration is what people expect until they say otherwise, and it stays editable.
	slotInterval := req.DurationMinutes
	if req.SlotIntervalMinutes != nil {
		slotInterval = *req.SlotIntervalMinutes
	}
	if slotInterval <= 0 {
		h.writeError(w, http.StatusBadRequest, "slot_interval_minutes must be positive")
		return
	}
	// Location is usually omitted at create (the quick-create form only sets
	// slug/name/duration) and configured later in the editor. When omitted, pick a
	// smart default from the owner's connected calendar; only validate the location
	// when the caller explicitly set it.
	var locType string
	if req.LocationType != nil {
		locType = *req.LocationType
		if err := h.validateLocation(r.Context(), user.ID, locType, req.LocationValue); err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		locType = h.smartDefaultLocation(r.Context(), user.ID)
	}
	routingMode := "fixed"
	if req.RoutingMode != nil {
		routingMode = *req.RoutingMode
	}
	bufBefore, bufAfter, minNotice, maxFuture := 0, 0, 0, 60
	if req.BufferBeforeMinutes != nil {
		bufBefore = *req.BufferBeforeMinutes
	}
	if req.BufferAfterMinutes != nil {
		bufAfter = *req.BufferAfterMinutes
	}
	if req.MinNoticeMinutes != nil {
		minNotice = *req.MinNoticeMinutes
	}
	if req.MaxFutureDays != nil {
		maxFuture = *req.MaxFutureDays
	}
	maxActive := 1
	if req.MaxActiveBookings != nil {
		maxActive = *req.MaxActiveBookings
	}
	// Off unless explicitly asked for: showing booked times makes a host's calendar
	// legible through a public endpoint, so it is never inherited by default (#19).
	showTaken := 0
	if req.ShowTakenSlots != nil && *req.ShowTakenSlots {
		showTaken = 1
	}

	id := uid.New()
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create event type: begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO event_types
		  (id, user_id, slug, name, description, duration_minutes,
		   slot_interval_minutes, location_type, location_value, allow_phone_call,
		   routing_mode, buffer_before_minutes, buffer_after_minutes,
		   min_notice_minutes, max_future_days, max_active_bookings, show_taken_slots,
		   msg_confirmation, msg_cancellation, msg_reschedule, msg_reminder, visibility)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, user.ID, slug, req.Name, req.Description,
		req.DurationMinutes, slotInterval, locType, req.LocationValue, req.AllowPhoneCall != nil && *req.AllowPhoneCall,
		routingMode, bufBefore, bufAfter, minNotice, maxFuture, maxActive, showTaken,
		defaultMsgConfirmation, defaultMsgCancellation, defaultMsgReschedule, defaultMsgReminder, visibility)
	if err != nil {
		if db.IsUniqueViolation(err) {
			h.writeError(w, http.StatusConflict, "slug already in use")
			return
		}
		if db.IsCheckViolation(err) {
			h.writeError(w, http.StatusBadRequest, "invalid location_type or routing_mode value")
			return
		}
		h.logger.ErrorContext(r.Context(), "create event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Seed the owner as the single required host (Normal). Keeps host resolution
	// uniform — every event type has at least one host from creation.
	if _, err = tx.ExecContext(r.Context(), `
		INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority)
		VALUES (?, ?, ?, 'required', 0)`, uid.New(), id, user.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "create event type: seed host", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err = tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "create event type: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	row := h.db.QueryRowContext(r.Context(), selectETCols+" WHERE id = ?", id)
	et, err := scanEventType(row)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "fetch created event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	et.OwnerName, et.OwnerEmail = user.Name, user.Email
	et.stampViewer(user) // the creator owns it
	h.writeJSON(w, http.StatusCreated, et)
}

// ListEventTypes handles GET /v1/event-types.
func (h *Handler) ListEventTypes(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())

	rows, err := h.db.QueryContext(r.Context(), listEventTypesQuery, user.ID, user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list event types", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	items := make([]eventTypeJSON, 0)
	for rows.Next() {
		var archived int
		var ownerName, ownerEmail sql.NullString
		et, err := scanEventTypeRow(rows, &ownerName, &ownerEmail, &archived)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "scan event type", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		et.OwnerName, et.OwnerEmail = ownerName.String, ownerEmail.String
		et.Archived = archived != 0
		et.stampViewer(user)
		items = append(items, *et)
	}
	if err := rows.Err(); err != nil {
		h.logger.ErrorContext(r.Context(), "list event types rows", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetEventType handles GET /v1/event-types/{slug}.
func (h *Handler) GetEventType(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	slug := r.PathValue("slug")

	var archived int
	var ownerName, ownerEmail sql.NullString
	row := h.db.QueryRowContext(r.Context(), getEventTypeQuery, slug, user.ID, user.ID)
	et, err := scanEventTypeRow(row, &ownerName, &ownerEmail, &archived)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "get event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if et == nil {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return
	}
	et.OwnerName, et.OwnerEmail = ownerName.String, ownerEmail.String
	et.Archived = archived != 0
	et.stampViewer(user)
	if err := h.loadReminders(r.Context(), et.ID, et); err != nil {
		h.logger.ErrorContext(r.Context(), "get event type: load reminders", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeJSON(w, http.StatusOK, et)
}

// PatchEventType handles PATCH /v1/event-types/{slug}.
func (h *Handler) PatchEventType(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	slug := r.PathValue("slug")
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)

	var req struct {
		Slug                *string `json:"slug"`
		Name                *string `json:"name"`
		Description         *string `json:"description"`
		DurationMinutes     *int    `json:"duration_minutes"`
		SlotIntervalMinutes *int    `json:"slot_interval_minutes"`
		LocationType        *string `json:"location_type"`
		LocationValue       *string `json:"location_value"`
		RoutingMode         *string `json:"routing_mode"`
		RRStrategy          *string `json:"rr_strategy"`
		BufferBeforeMinutes *int    `json:"buffer_before_minutes"`
		BufferAfterMinutes  *int    `json:"buffer_after_minutes"`
		MinNoticeMinutes    *int    `json:"min_notice_minutes"`
		MaxFutureDays       *int    `json:"max_future_days"`
		MaxActiveBookings   *int    `json:"max_active_bookings"`
		IsActive            *bool   `json:"is_active"`
		IsPublic            *bool   `json:"is_public"`
		AllowPhoneCall      *bool   `json:"allow_phone_call"`
		ShowTakenSlots      *bool   `json:"show_taken_slots"`
		Archived            *bool   `json:"archived"`
		MsgConfirmation     *string `json:"msg_confirmation"`
		MsgCancellation     *string `json:"msg_cancellation"`
		MsgReschedule       *string `json:"msg_reschedule"`
		MsgReminder         *string `json:"msg_reminder"`
		MsgGreeting         *string `json:"msg_greeting"`
		SubjConfirmation    *string `json:"subj_confirmation"`
		SubjCancellation    *string `json:"subj_cancellation"`
		SubjReschedule      *string `json:"subj_reschedule"`
		SubjReminder        *string `json:"subj_reminder"`
		CalendarMessage     *string `json:"calendar_message"`
		PriceCents          *int    `json:"price_cents"`
		Currency            *string `json:"currency"`
		Visibility          *string `json:"visibility"`
		Reminders           []int   `json:"reminders"` // nil = don't touch; [] = clear all
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Who may write is decided once, here, by the shared rule (owner, or admin on an
	// org-wide event type). Everything below keys on the id it returns.
	ref := h.eventTypeForEditor(w, r, slug, user)
	if ref == nil {
		return
	}
	etID := ref.ID

	// Validate reminders list before touching the DB.
	if req.Reminders != nil {
		for _, hb := range req.Reminders {
			if !validReminderHours[hb] {
				h.writeError(w, http.StatusBadRequest, "reminder hours_before must be one of: 1, 2, 4, 8, 12, 24, 48, 72, 168")
				return
			}
		}
		if len(req.Reminders) > 5 {
			h.writeError(w, http.StatusBadRequest, "at most 5 reminders per event type")
			return
		}
	}

	var setClauses []string
	var args []any
	set := func(col string, val any) {
		setClauses = append(setClauses, col+" = ?")
		args = append(args, val)
	}

	if req.Name != nil {
		if *req.Name == "" {
			h.writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		set("name", *req.Name)
	}
	if req.Description != nil {
		set("description", *req.Description)
	}
	if req.DurationMinutes != nil {
		if *req.DurationMinutes <= 0 {
			h.writeError(w, http.StatusBadRequest, "duration_minutes must be positive")
			return
		}
		set("duration_minutes", *req.DurationMinutes)
	}
	if req.SlotIntervalMinutes != nil {
		// slots.Generate refuses a non-positive interval, so an unvalidated 0 here would
		// leave the event type with no bookable times at all and no clue why.
		if *req.SlotIntervalMinutes <= 0 {
			h.writeError(w, http.StatusBadRequest, "slot_interval_minutes must be positive")
			return
		}
		set("slot_interval_minutes", *req.SlotIntervalMinutes)
	}
	if req.LocationType != nil {
		set("location_type", *req.LocationType)
	}
	if req.LocationValue != nil {
		set("location_value", *req.LocationValue)
	}
	if req.RoutingMode != nil {
		switch *req.RoutingMode {
		case "fixed", "round_robin", "collective":
			set("routing_mode", *req.RoutingMode)
		default:
			h.writeError(w, http.StatusBadRequest, "routing_mode must be 'fixed', 'round_robin', or 'collective'")
			return
		}
	}
	if req.RRStrategy != nil {
		switch *req.RRStrategy {
		case "even", "soonest", "priority":
			set("rr_strategy", *req.RRStrategy)
		default:
			h.writeError(w, http.StatusBadRequest, "rr_strategy must be 'even', 'soonest', or 'priority'")
			return
		}
	}
	if req.BufferBeforeMinutes != nil {
		set("buffer_before_minutes", *req.BufferBeforeMinutes)
	}
	if req.BufferAfterMinutes != nil {
		set("buffer_after_minutes", *req.BufferAfterMinutes)
	}
	if req.MinNoticeMinutes != nil {
		set("min_notice_minutes", *req.MinNoticeMinutes)
	}
	if req.MaxFutureDays != nil {
		set("max_future_days", *req.MaxFutureDays)
	}
	if req.MaxActiveBookings != nil {
		if *req.MaxActiveBookings < 0 {
			h.writeError(w, http.StatusBadRequest, "max_active_bookings cannot be negative (0 = unlimited)")
			return
		}
		set("max_active_bookings", *req.MaxActiveBookings)
	}
	if req.PriceCents != nil {
		if *req.PriceCents < 0 {
			h.writeError(w, http.StatusBadRequest, "price_cents cannot be negative (0 = free)")
			return
		}
		// A price is only bookable once payments are configured — block the footgun of a
		// paid event type that no one can actually book.
		if *req.PriceCents > 0 && h.getStripe() == nil {
			h.writeError(w, http.StatusBadRequest, "connect Stripe in Settings → Payments before setting a price")
			return
		}
		set("price_cents", *req.PriceCents)
	}
	if req.Currency != nil {
		cur := strings.ToLower(strings.TrimSpace(*req.Currency))
		if len(cur) != 3 {
			h.writeError(w, http.StatusBadRequest, "currency must be a 3-letter ISO 4217 code")
			return
		}
		set("currency", cur)
	}
	if req.IsActive != nil {
		v := 0
		if *req.IsActive {
			v = 1
		}
		set("is_active", v)
	}
	if req.IsPublic != nil {
		v := 0
		if *req.IsPublic {
			v = 1
		}
		set("is_public", v)
	}
	if req.AllowPhoneCall != nil {
		set("allow_phone_call", *req.AllowPhoneCall)
	}
	if req.ShowTakenSlots != nil {
		v := 0
		if *req.ShowTakenSlots {
			v = 1
		}
		set("show_taken_slots", v)
	}
	if req.Visibility != nil {
		if !validEventTypeVisibility(*req.Visibility) {
			h.writeError(w, http.StatusBadRequest, "visibility must be 'org' or 'private'")
			return
		}
		// Whether the rest of the workspace may see it is the owner's call. An admin
		// may edit an org-wide event type, but not hide it from (or on behalf of) the
		// person who made it. Compared against the stored value, not merely mentioned:
		// the editor resubmits the whole form, and an admin saving an unrelated field
		// must not be refused for a visibility they did not touch.
		if *req.Visibility != ref.Visibility {
			if user.ID != ref.OwnerID {
				h.writeError(w, http.StatusForbidden, "only the owner can change who an event type is visible to")
				return
			}
			set("visibility", *req.Visibility)
		}
	}
	if req.Archived != nil {
		if *req.Archived {
			// strftime literal (no bound value) — matches the DB's timestamp format;
			// also force is_active off so the archived type stops taking bookings.
			setClauses = append(setClauses, "archived_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')")
			set("is_active", 0)
		} else {
			setClauses = append(setClauses, "archived_at = NULL")
		}
	}
	const maxMsgLen = 2000
	// Email notes are rich text (sanitized HTML) since the editor moved to a rich
	// editor; markup counts toward the cap, so it is roomier than the plain fields'.
	const maxNoteLen = 4000
	if req.CalendarMessage != nil {
		// Sanitized on the way in (and again on send — see richtext): the stored value
		// is the allowlisted HTML, so a later GET round-trips exactly what will be sent.
		// The length cap applies to the raw submission, so it cannot be dodged by
		// padding with markup that the sanitizer strips.
		if len(*req.CalendarMessage) > maxCalendarMessageLen {
			h.writeError(w, http.StatusBadRequest, "calendar_message exceeds 10000 characters")
			return
		}
		set("calendar_message", nullableString(richtext.Sanitize(*req.CalendarMessage)))
	}
	if req.MsgConfirmation != nil {
		if len(*req.MsgConfirmation) > maxNoteLen {
			h.writeError(w, http.StatusBadRequest, "msg_confirmation exceeds 4000 characters")
			return
		}
		set("msg_confirmation", nullableString(richtext.Sanitize(*req.MsgConfirmation)))
	}
	if req.MsgCancellation != nil {
		if len(*req.MsgCancellation) > maxNoteLen {
			h.writeError(w, http.StatusBadRequest, "msg_cancellation exceeds 4000 characters")
			return
		}
		set("msg_cancellation", nullableString(richtext.Sanitize(*req.MsgCancellation)))
	}
	if req.MsgReschedule != nil {
		if len(*req.MsgReschedule) > maxNoteLen {
			h.writeError(w, http.StatusBadRequest, "msg_reschedule exceeds 4000 characters")
			return
		}
		set("msg_reschedule", nullableString(richtext.Sanitize(*req.MsgReschedule)))
	}
	if req.MsgReminder != nil {
		if len(*req.MsgReminder) > maxNoteLen {
			h.writeError(w, http.StatusBadRequest, "msg_reminder exceeds 4000 characters")
			return
		}
		set("msg_reminder", nullableString(richtext.Sanitize(*req.MsgReminder)))
	}
	if req.MsgGreeting != nil {
		if len(*req.MsgGreeting) > maxMsgLen {
			h.writeError(w, http.StatusBadRequest, "msg_greeting exceeds 2000 characters")
			return
		}
		set("msg_greeting", nullableString(*req.MsgGreeting))
	}
	const maxSubjLen = 200
	for _, s := range []struct {
		col string
		val *string
	}{
		{"subj_confirmation", req.SubjConfirmation},
		{"subj_cancellation", req.SubjCancellation},
		{"subj_reschedule", req.SubjReschedule},
		{"subj_reminder", req.SubjReminder},
	} {
		if s.val == nil {
			continue
		}
		if len(*s.val) > maxSubjLen {
			h.writeError(w, http.StatusBadRequest, s.col+" exceeds 200 characters")
			return
		}
		set(s.col, nullableString(strings.TrimSpace(*s.val)))
	}

	// Current location, to validate the effective online-meeting location below.
	var curLocType, curLocVal string
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT location_type, COALESCE(location_value, '') FROM event_types WHERE id = ?`, etID).
		Scan(&curLocType, &curLocVal); err != nil {
		if err == sql.ErrNoRows {
			h.writeError(w, http.StatusNotFound, "event type not found")
			return
		}
		h.logger.ErrorContext(r.Context(), "patch event type: lookup location", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Validate the location only when this patch actually CHANGES it, comparing what
	// will be in effect against what is stored - not merely when the request mentions
	// the fields.
	//
	// "Mentions" was the old test, and the editor mentions them on every save: it submits
	// the whole form, so validation ran against fields the operator had not touched. Any
	// event type already holding a location the current rules reject was therefore
	// unsaveable from the UI, whatever you were actually trying to edit, with an error
	// about a meeting URL you never went near. Rows reach that state legitimately - a
	// create that defaulted the location before smartDefaultLocation was fixed, a
	// provider disconnected since, a duplicate that inherited it (#22), or the demo seed.
	//
	// This is the general form of the rule CLAUDE.md records for the slot-interval floor:
	// a stored value the editor cannot re-submit locks the operator out of every other
	// field. Editing the location still validates, so the state is fixable, and there is
	// no path that writes a NEW invalid value.
	effLocType := curLocType
	if req.LocationType != nil {
		effLocType = *req.LocationType
	}
	effLocVal := curLocVal
	if req.LocationValue != nil {
		effLocVal = *req.LocationValue
	}
	//
	// Validated against the OWNER's connections, not the caller's: an auto-generated
	// Meet/Teams/Zoom link is minted from the owner's calendar at booking time, so an
	// admin editing someone else's event type must be held to what that owner can host.
	if effLocType != curLocType || effLocVal != curLocVal {
		if err := h.validateLocation(r.Context(), ref.OwnerID, effLocType, &effLocVal); err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// The slug is the public booking URL, so renaming one that is already in circulation
	// breaks every link to it: an invitation in somebody's inbox, a page embedding the
	// widget, a QR code on a card. It is allowed only while the event type has no
	// bookings, which is exactly the case that needs it - a fresh duplicate arrives as
	// "<slug>-copy" and there is otherwise no way to give it a real name (#22).
	//
	// "No bookings" rather than "not yet active": a link can be shared before anyone
	// books, but a booking is the first evidence the URL actually reached someone, and it
	// is the check we can make honestly. Cancelled ones count - the manage link in that
	// booker's confirmation email still resolves through the slug.
	if req.Slug != nil {
		newSlug := slugify(*req.Slug)
		if newSlug == "" {
			h.writeError(w, http.StatusBadRequest,
				"slug cannot be empty (letters and digits only, joined by hyphens)")
			return
		}
		// A rename is a change from the STORED slug, compared in canonical form. A row
		// the boot sweep left alone (non-canonical, with bookings - see
		// NormalizeEventTypeSlugs) is resubmitted by the editor exactly as stored, and
		// "Intro_Call" vs "intro-call" is not the operator asking for a rename; refusing
		// it would lock every other field on that event type (CLAUDE.md: validate on
		// change, not on mention).
		if newSlug != slug && newSlug != slugify(slug) {
			var bookings int
			if err := h.db.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM bookings WHERE event_type_id = ?`, etID).Scan(&bookings); err != nil {
				h.logger.ErrorContext(r.Context(), "patch event type: count bookings", "error", err)
				h.writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if bookings > 0 {
				h.writeError(w, http.StatusConflict,
					"cannot change the slug of an event type that already has bookings - "+
						"its booking links are already in circulation")
				return
			}
			set("slug", newSlug)
		}
	}

	// Apply the event_types UPDATE if there are scalar fields to change.
	if len(setClauses) > 0 {
		args = append(args, etID)
		res, err := h.db.ExecContext(r.Context(),
			"UPDATE event_types SET "+strings.Join(setClauses, ", ")+" WHERE id = ?", // #nosec G202 -- setClauses is built by set()/the literal col list above; every column name is a hardcoded string, every value is bound via args...
			args...)
		if err != nil {
			if db.IsUniqueViolation(err) {
				h.writeError(w, http.StatusConflict, "slug already in use")
				return
			}
			if db.IsCheckViolation(err) {
				h.writeError(w, http.StatusBadRequest, "invalid location_type or routing_mode value")
				return
			}
			h.logger.ErrorContext(r.Context(), "patch event type", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			h.writeError(w, http.StatusNotFound, "event type not found")
			return
		}
	}

	// Replace the reminders list atomically if the caller sent it.
	if req.Reminders != nil {
		if err := h.replaceReminders(r.Context(), etID, req.Reminders); err != nil {
			h.logger.ErrorContext(r.Context(), "patch event type: replace reminders", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// Re-read by id: a rename above moved the row out from under the slug this request
	// arrived on, and re-reading by that name would 404 a patch that succeeded.
	et, err := h.fetchEventTypeByID(r.Context(), etID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "fetch patched event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if et == nil {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return
	}
	et.stampViewer(user)
	h.writeJSON(w, http.StatusOK, et)
}

// fetchEventTypeByID loads one event type with its owner's name/email and reminders,
// in the shape GET returns. Access is the caller's business; (nil, nil) if missing.
func (h *Handler) fetchEventTypeByID(ctx context.Context, id string) (*eventTypeJSON, error) {
	var archived int
	var ownerName, ownerEmail sql.NullString
	row := h.db.QueryRowContext(ctx, "SELECT "+etOwnerColumns+" FROM event_types WHERE id = ?", id)
	et, err := scanEventTypeRow(row, &ownerName, &ownerEmail, &archived)
	if err != nil || et == nil {
		return et, err
	}
	et.OwnerName, et.OwnerEmail = ownerName.String, ownerEmail.String
	et.Archived = archived != 0
	if err := h.loadReminders(ctx, id, et); err != nil {
		return nil, err
	}
	return et, nil
}

// nullableString converts an empty string to nil (NULL in SQLite) so clearing a
// custom note stores NULL rather than an empty string.
// maxCalendarMessageLen bounds the raw calendar_message submission. Larger than the
// email notes' 2000 because markup counts and the invite body is a natural home for an
// agenda, but still small enough that Google's description limit is never in play.
const maxCalendarMessageLen = 10000

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// replaceReminders deletes all existing reminder rows for etID and inserts newHours.
func (h *Handler) replaceReminders(ctx context.Context, etID string, newHours []int) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM event_type_reminders WHERE event_type_id = ?`, etID); err != nil {
		return err
	}
	for _, hb := range newHours {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_type_reminders (id, event_type_id, hours_before) VALUES (?, ?, ?)`,
			uid.New(), etID, hb); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteEventType handles DELETE /v1/event-types/{slug}.
func (h *Handler) DeleteEventType(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	etID := h.eventTypeIDForEditor(w, r, r.PathValue("slug"), user)
	if etID == "" {
		return
	}

	// Upcoming active bookings block deletion: someone is still expecting that meeting, so
	// they must be cancelled or moved first. Past and cancelled bookings are history and
	// go with the event type (their answers, hosts, tokens and messages cascade; webhook
	// delivery logs are kept but detached from the booking).
	ctx := r.Context()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var upcoming int
	if err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bookings
		WHERE event_type_id = ? AND status != 'cancelled' AND end_at > ?`, etID, now).Scan(&upcoming); err != nil {
		h.logger.ErrorContext(ctx, "delete event type: count upcoming", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if upcoming > 0 {
		h.writeError(w, http.StatusConflict, fmt.Sprintf(
			"this event type has %d upcoming booking(s) — cancel or reschedule them first, or deactivate it instead", upcoming))
		return
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.logger.ErrorContext(ctx, "delete event type: begin", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
		UPDATE webhook_deliveries SET booking_id = NULL
		WHERE booking_id IN (SELECT id FROM bookings WHERE event_type_id = ?)`, etID); err != nil {
		h.logger.ErrorContext(ctx, "delete event type: detach webhook deliveries", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bookings WHERE event_type_id = ?`, etID); err != nil {
		h.logger.ErrorContext(ctx, "delete event type: delete past bookings", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM event_types WHERE id = ?`, etID)
	if err != nil {
		if db.IsForeignKeyViolation(err) {
			h.writeError(w, http.StatusConflict, "this event type is still referenced and can't be deleted — deactivate it instead")
			return
		}
		h.logger.ErrorContext(ctx, "delete event type", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		h.writeError(w, http.StatusNotFound, "event type not found")
		return
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(ctx, "delete event type: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
