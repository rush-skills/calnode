package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/slots"
)

// liveCalendar is a Google-shaped fake: it records every CreateEvent's params and every
// update/cancel, and mints a Meet-style link when AddMeet is set.
type liveCalendar struct {
	calendar.Provider
	noLink  bool // write the event but mint no link (a personal Microsoft account)
	mu      sync.Mutex
	creates []calendar.CreateEventParams
	updates [][2]time.Time
	cancels []string
}

func (p *liveCalendar) Name() string                                         { return "google" }
func (p *liveCalendar) InvitesGuests() bool                                  { return true }
func (p *liveCalendar) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *liveCalendar) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *liveCalendar) CreateEvent(_ context.Context, _ string, in calendar.CreateEventParams) (string, string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, in)
	link := ""
	if in.AddMeet && !p.noLink {
		link = "https://meet.google.com/abc-defg-hij"
	}
	return "evt-" + in.Summary, link, "primary", nil
}
func (p *liveCalendar) UpdateEvent(_ context.Context, _, _, _ string, start, end time.Time, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, [2]time.Time{start, end})
	return nil
}
func (p *liveCalendar) CancelEvent(_ context.Context, _, _, eventID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, eventID)
	return nil
}

func (p *liveCalendar) snapshot() (creates []calendar.CreateEventParams, updates [][2]time.Time, cancels []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]calendar.CreateEventParams(nil), p.creates...), append([][2]time.Time(nil), p.updates...), append([]string(nil), p.cancels...)
}

// connectLiveCalendar gives userID a Google destination calendar served by a liveCalendar.
func connectLiveCalendar(t *testing.T, h *handler.Handler, db *sql.DB, userID string) *liveCalendar {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES (?,?,'google','test','primary',1)`,
		"conn-"+userID, userID); err != nil {
		t.Fatal(err)
	}
	p := &liveCalendar{}
	svc := calendar.NewService(db)
	svc.Register(p)
	h.SetCalendar(svc)
	return p
}

// seedLiveMember inserts an active member with an API key and returns the key.
func seedLiveMember(t *testing.T, db *sql.DB, id, email string, admin bool) string {
	t.Helper()
	key := "key-" + id
	mustExec(t, db, `INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES (?,?,?,'UTC',?)`, id, email, "User "+id, admin)
	mustExec(t, db, `INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES (?,?,'t',?,'2024-01-01')`, "k-"+id, id, sha256HexForTest(key))
	return key
}

// liveReq dispatches one live-events API call through RequireAuth, with {id} bound.
func liveReq(h *handler.Handler, key, method, path, id, body string) *httptest.ResponseRecorder {
	req := authReq(method, path, body, key)
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	var fn http.HandlerFunc
	switch {
	case method == http.MethodPost && id == "":
		fn = h.CreateLiveEvent
	case method == http.MethodGet && id == "":
		fn = h.ListLiveEvents
	case method == http.MethodGet:
		fn = h.GetLiveEvent
	case method == http.MethodPatch:
		fn = h.PatchLiveEvent
	case method == http.MethodDelete:
		fn = h.CancelLiveEvent
	case strings.HasSuffix(path, "/start"):
		fn = h.StartLiveEvent
	case strings.HasSuffix(path, "/end"):
		fn = h.EndLiveEvent
	}
	h.RequireAuth(fn)(rec, req)
	return rec
}

func liveBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	return m
}

func liveStatusBody(t *testing.T, h *handler.Handler, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.LiveStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/live/status"+query, nil))
	mustStatus(t, rec, http.StatusOK, "live status")
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status headers = %v; want CORS * and no-store", rec.Header())
	}
	return liveBody(t, rec)
}

// ---------------------------------------------------------------------------
// State machine
// ---------------------------------------------------------------------------

func TestLiveEvents_scheduledStartEndCancel(t *testing.T) {
	h, _, key, userID := setupWorkspaceWithDB(t)

	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "",
		`{"title":"Office hours","description":"Drop in.","join_url":"https://meet.example.com/x","scheduled_start_at":"2030-01-01T10:00:00Z","scheduled_end_at":"2030-01-01T11:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	ev := liveBody(t, rec)
	id := ev["id"].(string)
	if ev["status"] != "scheduled" || ev["kind"] != "office_hours" || ev["host_user_id"] != userID || ev["host_name"] != "Test Host" {
		t.Fatalf("created row = %v", ev)
	}
	if ev["auto_start"] != true || ev["auto_end"] != true || ev["has_calendar_event"] != false {
		t.Fatalf("defaults = %v", ev)
	}

	// Ending a session that never went live is a conflict; cancel is the way out.
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/end", id, ""), http.StatusConflict, "end scheduled")

	rec = liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, "")
	mustStatus(t, rec, http.StatusOK, "start")
	ev = liveBody(t, rec)
	if ev["status"] != "live" || ev["started_at"] == nil || ev["join_url"] != "https://meet.example.com/x" {
		t.Fatalf("after start = %v", ev)
	}
	// Idempotent.
	rec = liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, "")
	mustStatus(t, rec, http.StatusOK, "start again")
	if liveBody(t, rec)["started_at"] != ev["started_at"] {
		t.Error("second start changed started_at")
	}

	rec = liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/end", id, "")
	mustStatus(t, rec, http.StatusOK, "end")
	ev = liveBody(t, rec)
	if ev["status"] != "ended" || ev["ended_at"] == nil {
		t.Fatalf("after end = %v", ev)
	}
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/end", id, ""), http.StatusOK, "end again")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "restart ended")
	mustStatus(t, liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"title":"x"}`), http.StatusConflict, "edit ended")

	rec = liveReq(h, key, http.MethodDelete, "/v1/live-events/"+id, id, "")
	mustStatus(t, rec, http.StatusOK, "cancel")
	if liveBody(t, rec)["status"] != "cancelled" {
		t.Fatal("delete did not cancel")
	}
	mustStatus(t, liveReq(h, key, http.MethodDelete, "/v1/live-events/"+id, id, ""), http.StatusOK, "cancel again")
}

func TestLiveEvents_cancelLiveEndsItFirst(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Now","join_url":"https://x.example/y","start_now":true}`)
	mustStatus(t, rec, http.StatusCreated, "start_now")
	ev := liveBody(t, rec)
	if ev["status"] != "live" || ev["started_at"] == nil {
		t.Fatalf("start_now row = %v", ev)
	}
	id := ev["id"].(string)
	rec = liveReq(h, key, http.MethodDelete, "/v1/live-events/"+id, id, "")
	mustStatus(t, rec, http.StatusOK, "cancel live")
	ev = liveBody(t, rec)
	if ev["status"] != "cancelled" || ev["ended_at"] == nil {
		t.Fatalf("cancelled live row = %v (ended_at must be stamped)", ev)
	}
	if live := liveStatusBody(t, h, "")["live"].([]any); len(live) != 0 {
		t.Fatalf("cancelled session still published: %v", live)
	}
}

func TestLiveEvents_validation(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	cases := map[string]string{
		"no title":      `{"description":"x"}`,
		"bad kind":      `{"title":"t","kind":"Not A Slug!"}`,
		"bad join_url":  `{"title":"t","join_url":"javascript:alert(1)"}`,
		"bad time":      `{"title":"t","scheduled_start_at":"tomorrow"}`,
		"end<=start":    `{"title":"t","scheduled_start_at":"2030-01-01T10:00:00Z","scheduled_end_at":"2030-01-01T10:00:00Z"}`,
		"invalid json":  `{`,
		"member assign": `{"title":"t","host_user_id":"someone-else"}`,
	}
	for name, body := range cases {
		rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d; want 400 — %s", name, rec.Code, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Calendar + join link
// ---------------------------------------------------------------------------

func TestLiveEvents_startNowWithoutCalendarOrLinkIs409AndLeavesNoRow(t *testing.T) {
	h, db, key, _ := setupWorkspaceWithDB(t)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Now","start_now":true}`)
	mustStatus(t, rec, http.StatusConflict, "start_now without calendar")
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM live_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after failed start_now = %d (err %v); want 0", n, err)
	}

	// A scheduled session is fine without either; starting it later is the 409.
	rec = liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Later","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "scheduled without calendar")
	id := liveBody(t, rec)["id"].(string)
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "start without link")
	// Supplying a link unblocks it.
	mustStatus(t, liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"join_url":"https://meet.example.com/late"}`), http.StatusOK, "set link")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusOK, "start with link")
}

func TestLiveEvents_scheduledCreateMintsMeetAheadOfTime(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	p := connectLiveCalendar(t, h, db, userID)

	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "",
		`{"title":"Office hours","description":"Bring questions","scheduled_start_at":"2030-01-01T10:00:00Z","scheduled_end_at":"2030-01-01T11:30:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	ev := liveBody(t, rec)
	if ev["has_calendar_event"] != true || ev["join_url"] != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("scheduled create should mint the calendar event + Meet link up front: %v", ev)
	}
	creates, _, _ := p.snapshot()
	if len(creates) != 1 {
		t.Fatalf("creates = %d; want 1", len(creates))
	}
	c := creates[0]
	if !c.AddMeet || c.Summary != "Office hours" || c.Description != "Bring questions" {
		t.Errorf("create params = %+v", c)
	}
	if !c.Start.Equal(time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)) || !c.End.Equal(time.Date(2030, 1, 1, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("calendar window = %v–%v; want the scheduled window", c.Start, c.End)
	}
	id := ev["id"].(string)

	// Moving the schedule moves the calendar event.
	mustStatus(t, liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"scheduled_start_at":"2030-01-02T10:00:00Z","scheduled_end_at":"2030-01-02T11:00:00Z"}`), http.StatusOK, "reschedule")
	_, updates, _ := p.snapshot()
	if len(updates) != 1 || !updates[0][0].Equal(time.Date(2030, 1, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("updates after reschedule = %v", updates)
	}

	// Start reuses the event (no second create); end trims it to now; cancel deletes it.
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusOK, "start")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/end", id, ""), http.StatusOK, "end")
	creates, updates, cancels := p.snapshot()
	if len(creates) != 1 {
		t.Errorf("start created a second calendar event")
	}
	if len(updates) != 2 || time.Since(updates[1][1]) > time.Minute {
		t.Errorf("end should set the calendar end to now; updates = %v", updates)
	}
	if len(cancels) != 0 {
		t.Errorf("end must not delete the calendar event")
	}

	// A cancelled scheduled session deletes its calendar event.
	rec = liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Later","scheduled_start_at":"2030-02-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create second")
	id2 := liveBody(t, rec)["id"].(string)
	mustStatus(t, liveReq(h, key, http.MethodDelete, "/v1/live-events/"+id2, id2, ""), http.StatusOK, "cancel second")
	if _, _, cancels = p.snapshot(); len(cancels) != 1 || cancels[0] != "evt-Later" {
		t.Errorf("cancels = %v; want the stamped event id", cancels)
	}
}

func TestLiveEvents_manualJoinURLOverridesMint(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	p := connectLiveCalendar(t, h, db, userID)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Zoom one","join_url":"https://zoom.example/j/1","start_now":true}`)
	mustStatus(t, rec, http.StatusCreated, "start_now")
	ev := liveBody(t, rec)
	if ev["join_url"] != "https://zoom.example/j/1" || ev["has_calendar_event"] != true {
		t.Fatalf("row = %v; want the manual link kept and a calendar event written", ev)
	}
	creates, _, _ := p.snapshot()
	if len(creates) != 1 || creates[0].AddMeet || creates[0].Location != "https://zoom.example/j/1" {
		t.Fatalf("create params = %+v; want AddMeet=false and the manual link as Location", creates)
	}
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

func TestLiveEvents_permissions(t *testing.T) {
	h, db, ownerKey, _ := setupWorkspaceWithDB(t)
	hostKey := seedLiveMember(t, db, "host", "host2@example.com", false)
	otherKey := seedLiveMember(t, db, "other", "other@example.com", false)
	adminKey := seedLiveMember(t, db, "admin2", "admin2@example.com", true)
	creatorKey := seedLiveMember(t, db, "creator", "creator@example.com", false)

	// Admin creates a session hosted by "host"; a member may not assign another host.
	mustStatus(t, liveReq(h, creatorKey, http.MethodPost, "/v1/live-events", "", `{"title":"t","host_user_id":"host"}`), http.StatusForbidden, "member assigns host")
	rec := liveReq(h, ownerKey, http.MethodPost, "/v1/live-events", "", `{"title":"t","host_user_id":"host","join_url":"https://x.example/1"}`)
	mustStatus(t, rec, http.StatusCreated, "admin creates")
	id := liveBody(t, rec)["id"].(string)

	// Any member can read.
	mustStatus(t, liveReq(h, otherKey, http.MethodGet, "/v1/live-events/"+id, id, ""), http.StatusOK, "other reads")
	mustStatus(t, liveReq(h, otherKey, http.MethodGet, "/v1/live-events", "", ""), http.StatusOK, "other lists")

	// An unrelated member cannot manage.
	for _, c := range []struct{ method, suffix, body string }{
		{http.MethodPatch, "", `{"title":"hijack"}`},
		{http.MethodPost, "/start", ""},
		{http.MethodPost, "/end", ""},
		{http.MethodDelete, "", ""},
	} {
		rec := liveReq(h, otherKey, c.method, "/v1/live-events/"+id+c.suffix, id, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("other member %s %s: status = %d; want 403", c.method, c.suffix, rec.Code)
		}
	}
	// The host can; so can another admin.
	mustStatus(t, liveReq(h, hostKey, http.MethodPatch, "/v1/live-events/"+id, id, `{"title":"host edit"}`), http.StatusOK, "host edits")
	mustStatus(t, liveReq(h, hostKey, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusOK, "host starts")
	mustStatus(t, liveReq(h, adminKey, http.MethodPost, "/v1/live-events/"+id+"/end", id, ""), http.StatusOK, "admin ends")

	// The creator (a member) manages their own.
	rec = liveReq(h, creatorKey, http.MethodPost, "/v1/live-events", "", `{"title":"mine","join_url":"https://x.example/2"}`)
	mustStatus(t, rec, http.StatusCreated, "member creates own")
	mine := liveBody(t, rec)["id"].(string)
	mustStatus(t, liveReq(h, creatorKey, http.MethodDelete, "/v1/live-events/"+mine, mine, ""), http.StatusOK, "creator cancels own")

	// Unknown id.
	for _, c := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodPatch, ""}, {http.MethodPost, "/start"}, {http.MethodPost, "/end"}, {http.MethodDelete, ""}} {
		rec := liveReq(h, ownerKey, c.method, "/v1/live-events/nope"+c.suffix, "nope", `{}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s nope%s: status = %d; want 404", c.method, c.suffix, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Public status
// ---------------------------------------------------------------------------

func TestLiveStatus_publishesJoinURLOnlyWhileLive(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	create := func(body string) string {
		rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", body)
		mustStatus(t, rec, http.StatusCreated, "create "+body)
		return liveBody(t, rec)["id"].(string)
	}
	liveID := create(`{"title":"Live one","join_url":"https://x.example/live","start_now":true}`)
	create(`{"title":"Third","kind":"event","join_url":"https://x.example/3","scheduled_start_at":"2030-01-03T10:00:00Z"}`)
	create(`{"title":"First","join_url":"https://x.example/1","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	create(`{"title":"Second","join_url":"https://x.example/2","scheduled_start_at":"2030-01-02T10:00:00Z"}`)
	create(`{"title":"Past","join_url":"https://x.example/p","scheduled_start_at":"2020-01-01T10:00:00Z","scheduled_end_at":"2020-01-01T11:00:00Z"}`)
	create(`{"title":"Unscheduled","join_url":"https://x.example/u"}`)

	body := liveStatusBody(t, h, "")
	live := body["live"].([]any)
	if len(live) != 1 {
		t.Fatalf("live = %v; want one", live)
	}
	l := live[0].(map[string]any)
	if l["id"] != liveID || l["join_url"] != "https://x.example/live" || l["host_name"] != "Test Host" || l["started_at"] == nil {
		t.Errorf("live row = %v", l)
	}
	next := body["next"].(map[string]any)
	if next["title"] != "First" || next["scheduled_start_at"] != "2030-01-01T10:00:00Z" {
		t.Errorf("next = %v; want First", next)
	}
	if _, leaked := next["join_url"]; leaked {
		t.Errorf("next must not carry join_url: %v", next)
	}
	up := body["upcoming"].([]any)
	if len(up) != 2 || up[0].(map[string]any)["title"] != "Second" || up[1].(map[string]any)["title"] != "Third" {
		t.Errorf("upcoming = %v; want Second, Third (past and unscheduled excluded, next not repeated)", up)
	}
	for _, u := range up {
		if _, leaked := u.(map[string]any)["join_url"]; leaked {
			t.Errorf("upcoming must not carry join_url: %v", u)
		}
	}

	// Kind filter.
	body = liveStatusBody(t, h, "?kind=event")
	if len(body["live"].([]any)) != 0 || body["next"].(map[string]any)["title"] != "Third" || len(body["upcoming"].([]any)) != 0 {
		t.Errorf("kind=event: %v", body)
	}
	rec := httptest.NewRecorder()
	h.LiveStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/live/status?kind=Not%20A%20Slug", nil))
	mustStatus(t, rec, http.StatusBadRequest, "bad kind")

	// Ending withdraws the link.
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+liveID+"/end", liveID, ""), http.StatusOK, "end")
	body = liveStatusBody(t, h, "")
	if len(body["live"].([]any)) != 0 {
		t.Errorf("ended session still live: %v", body["live"])
	}
	if body["next"] == nil {
		t.Error("next should still be set once offline")
	}
}

func TestLiveStatus_noSessionsIsEmptyNotNull(t *testing.T) {
	h, _, _, _ := setupWorkspaceWithDB(t)
	body := liveStatusBody(t, h, "")
	if body["live"] == nil || body["upcoming"] == nil || body["next"] != nil {
		t.Errorf("empty status = %v; want live:[], upcoming:[], next:null", body)
	}
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

func TestSweepLiveEvents_autoStartAndAutoEnd(t *testing.T) {
	h, db, key, _ := setupWorkspaceWithDB(t)
	create := func(body string) string {
		rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", body)
		mustStatus(t, rec, http.StatusCreated, "create "+body)
		return liveBody(t, rec)["id"].(string)
	}
	status := func(id string) (string, int) {
		var s string
		var autoStart int
		if err := db.QueryRow(`SELECT status, auto_start FROM live_events WHERE id = ?`, id).Scan(&s, &autoStart); err != nil {
			t.Fatal(err)
		}
		return s, autoStart
	}
	due := create(`{"title":"due","join_url":"https://x.example/1","scheduled_start_at":"2030-01-01T10:00:00Z","scheduled_end_at":"2030-01-01T11:00:00Z"}`)
	future := create(`{"title":"future","join_url":"https://x.example/2","scheduled_start_at":"2030-01-01T12:00:00Z"}`)
	manual := create(`{"title":"manual","join_url":"https://x.example/3","scheduled_start_at":"2030-01-01T10:00:00Z","auto_start":false}`)
	noLink := create(`{"title":"nolink","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	openEnded := create(`{"title":"open","join_url":"https://x.example/4","scheduled_start_at":"2030-01-01T09:00:00Z"}`)
	neverEnds := create(`{"title":"forever","join_url":"https://x.example/5","scheduled_start_at":"2030-01-01T09:00:00Z","auto_end":false}`)

	ctx := context.Background()
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}

	h.SweepLiveEvents(ctx, at("2030-01-01T10:00:30Z"))
	for id, want := range map[string]string{due: "live", future: "scheduled", manual: "scheduled", noLink: "scheduled", openEnded: "live", neverEnds: "live"} {
		if got, _ := status(id); got != want {
			t.Errorf("after 10:00 sweep, %s = %s; want %s", id, got, want)
		}
	}
	if _, autoStart := status(noLink); autoStart != 0 {
		t.Error("a session with no join link should have auto_start turned off rather than retried forever")
	}
	var startedAt string
	if err := db.QueryRow(`SELECT started_at FROM live_events WHERE id = ?`, due).Scan(&startedAt); err != nil || startedAt != "2030-01-01T10:00:30Z" {
		t.Errorf("started_at = %q (err %v); want the sweep clock", startedAt, err)
	}

	// Past the scheduled end: only "due" ends. The open-ended ones run up to 4h.
	h.SweepLiveEvents(ctx, at("2030-01-01T11:00:00Z"))
	for id, want := range map[string]string{due: "ended", openEnded: "live", neverEnds: "live", future: "scheduled"} {
		if got, _ := status(id); got != want {
			t.Errorf("after 11:00 sweep, %s = %s; want %s", id, got, want)
		}
	}
	// 4h after the 10:00:30 start of the open-ended one.
	h.SweepLiveEvents(ctx, at("2030-01-01T14:00:30Z"))
	for id, want := range map[string]string{openEnded: "ended", neverEnds: "live", future: "live"} {
		if got, _ := status(id); got != want {
			t.Errorf("after 14:00 sweep, %s = %s; want %s", id, got, want)
		}
	}
	var endedAt string
	if err := db.QueryRow(`SELECT ended_at FROM live_events WHERE id = ?`, due).Scan(&endedAt); err != nil || endedAt != "2030-01-01T11:00:00Z" {
		t.Errorf("ended_at = %q (err %v); want the sweep clock", endedAt, err)
	}
}

// ---------------------------------------------------------------------------
// Public page + widget
// ---------------------------------------------------------------------------

func TestLivePage_isFrameableAndUncached(t *testing.T) {
	h, _, _, _ := setupWorkspaceWithDB(t)
	rec := httptest.NewRecorder()
	h.LivePage(rec, httptest.NewRequest(http.MethodGet, "/live?kind=event&theme=dark", nil))
	mustStatus(t, rec, http.StatusOK, "live page")
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors *") {
		t.Errorf("CSP = %q; want frame-ancestors *", csp)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "" {
		t.Errorf("X-Frame-Options = %q; the live page must be frameable", xfo)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", cc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-theme="dark"`) || !strings.Contains(body, `/v1/live/status`) || !strings.Contains(body, `"event"`) {
		t.Errorf("page body missing theme/kind/polling wiring")
	}
}

func TestLiveWidgetJS_servedAsScript(t *testing.T) {
	h := &handler.Handler{}
	rec := httptest.NewRecorder()
	h.LiveWidgetJS(rec, httptest.NewRequest(http.MethodGet, "/live-widget.js", nil))
	mustStatus(t, rec, http.StatusOK, "widget")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("ETag") == "" {
		t.Errorf("widget headers = %v; want CORS * and an ETag", rec.Header())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `customElements.define('calnode-live'`) || !strings.Contains(body, "/v1/live/status") {
		t.Error("widget script lacks the component definition or status polling")
	}
	if strings.Contains(body, "BookingLogic") {
		t.Error("the widget must not reference BookingLogic (undefined on a host page)")
	}
	req := httptest.NewRequest(http.MethodGet, "/live-widget.js", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	h.LiveWidgetJS(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d; want 304", rec2.Code)
	}
}

// ---------------------------------------------------------------------------
// Review follow-ups
// ---------------------------------------------------------------------------

// A provider can write the event yet mint no link (a personal Microsoft account). The
// session must not go live on the strength of the stamped event, and a failed start_now
// must take that event back off the host's calendar along with the row.
func TestLiveEvents_stampedEventWithoutLinkNeverGoesLive(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	p := connectLiveCalendar(t, h, db, userID)
	p.noLink = true

	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Now","start_now":true}`)
	mustStatus(t, rec, http.StatusConflict, "start_now without a mintable link")
	creates, _, cancels := p.snapshot()
	if len(creates) != 1 || len(cancels) != 1 {
		t.Fatalf("creates=%d cancels=%d; the orphaned calendar event must be cancelled", len(creates), len(cancels))
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM live_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left = %d", n)
	}

	rec = liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Later","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "scheduled")
	ev := liveBody(t, rec)
	if ev["has_calendar_event"] != true || ev["join_url"] != "" {
		t.Fatalf("scheduled row = %v; want a stamped event and no link", ev)
	}
	id := ev["id"].(string)
	// Two starts in a row: the second sees the stamp and must still refuse.
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "start 1")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "start 2")
	if _, _, cancels = p.snapshot(); len(cancels) != 1 {
		t.Errorf("a scheduled session keeps its calendar event across a refused start (cancels=%d)", len(cancels))
	}
}

// Clearing the link of a scheduled session is allowed, but it then cannot start.
func TestLiveEvents_clearedLinkBlocksStart(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"t","join_url":"https://x.example/1"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	id := liveBody(t, rec)["id"].(string)
	mustStatus(t, liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"join_url":""}`), http.StatusOK, "clear link")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "start without link")
}

func TestLiveEvents_startNowRejectsPastEnd(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"t","join_url":"https://x.example/1","start_now":true,"scheduled_end_at":"2020-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusBadRequest, "past end with start_now")
}

// PATCH host_user_id: "" is "no change"; a live session keeps its host.
func TestLiveEvents_patchHostSemantics(t *testing.T) {
	h, db, ownerKey, _ := setupWorkspaceWithDB(t)
	seedLiveMember(t, db, "bob", "bob@example.com", false)
	rec := liveReq(h, ownerKey, http.MethodPost, "/v1/live-events", "", `{"title":"t","host_user_id":"bob","join_url":"https://x.example/1"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	id := liveBody(t, rec)["id"].(string)

	rec = liveReq(h, ownerKey, http.MethodPatch, "/v1/live-events/"+id, id, `{"host_user_id":"","title":"renamed"}`)
	mustStatus(t, rec, http.StatusOK, "patch with empty host")
	if ev := liveBody(t, rec); ev["host_user_id"] != "bob" || ev["title"] != "renamed" {
		t.Fatalf("empty host_user_id must not reassign: %v", ev)
	}
	mustStatus(t, liveReq(h, ownerKey, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusOK, "start")
	mustStatus(t, liveReq(h, ownerKey, http.MethodPatch, "/v1/live-events/"+id, id, `{"host_user_id":"host"}`), http.StatusConflict, "host change while live")
}

// Replacing a minted link with a manual one re-creates the calendar event with the manual
// link as its location, so the host and the notetaker land in the published room.
func TestLiveEvents_manualLinkOnMintedSessionRemintsCalendarEvent(t *testing.T) {
	h, db, key, userID := setupWorkspaceWithDB(t)
	p := connectLiveCalendar(t, h, db, userID)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"OH","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	id := liveBody(t, rec)["id"].(string)
	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"join_url":"https://zoom.example/j/9"}`)
	mustStatus(t, rec, http.StatusOK, "set manual link")
	if ev := liveBody(t, rec); ev["join_url"] != "https://zoom.example/j/9" || ev["has_calendar_event"] != true {
		t.Fatalf("row = %v", ev)
	}
	creates, _, cancels := p.snapshot()
	if len(cancels) != 1 || len(creates) != 2 || creates[1].AddMeet || creates[1].Location != "https://zoom.example/j/9" {
		t.Fatalf("creates=%+v cancels=%v; want the old event cancelled and a new one carrying the manual link", creates, cancels)
	}
}

// An open-ended scheduled session stays "next" for the running grace window after its
// start (auto_start off, host running late); one with a past scheduled end does not.
func TestLiveStatus_openEndedSessionStaysNextAfterStart(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	for _, body := range []string{
		`{"title":"late open","join_url":"https://x.example/1","scheduled_start_at":"` + recent + `","auto_start":false}`,
		`{"title":"stale open","join_url":"https://x.example/2","scheduled_start_at":"` + old + `","auto_start":false}`,
		`{"title":"ended window","join_url":"https://x.example/3","scheduled_start_at":"` + old + `","scheduled_end_at":"` + recent + `","auto_start":false}`,
	} {
		mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events", "", body), http.StatusCreated, "create")
	}
	body := liveStatusBody(t, h, "")
	next, _ := body["next"].(map[string]any)
	if next == nil || next["title"] != "late open" {
		t.Fatalf("next = %v; want the open-ended session that started an hour ago", body["next"])
	}
	if up := body["upcoming"].([]any); len(up) != 0 {
		t.Errorf("upcoming = %v; the stale and ended ones are over", up)
	}
}

// History lists ended and cancelled sessions only, filterable by kind and paged by offset;
// active lists scheduled and live.
func TestLiveEvents_listHistoryAndKindFilter(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	mk := func(title, kind string) string {
		rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "",
			`{"title":"`+title+`","kind":"`+kind+`","start_now":true,"join_url":"https://meet.example.com/`+title+`"}`)
		mustStatus(t, rec, http.StatusCreated, "create "+title)
		return liveBody(t, rec)["id"].(string)
	}
	a, b := mk("a", "office_hours"), mk("b", "demo")
	mk("c", "demo") // stays live
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+a+"/end", a, ""), http.StatusOK, "end a")
	mustStatus(t, liveReq(h, key, http.MethodDelete, "/v1/live-events/"+b, b, ""), http.StatusOK, "cancel b")

	count := func(q string) int {
		t.Helper()
		rec := liveReq(h, key, http.MethodGet, "/v1/live-events"+q, "", "")
		mustStatus(t, rec, http.StatusOK, q)
		return len(liveBody(t, rec)["live_events"].([]any))
	}
	cases := map[string]int{
		"?status=history":                   2,
		"?status=history&kind=demo":         1,
		"?status=history&kind=office_hours": 1,
		"?status=active":                    1,
		"?status=active&kind=office_hours":  0,
		"?status=history&limit=1&offset=1":  1,
		"?status=history&limit=1&offset=2":  0,
	}
	for q, want := range cases {
		if got := count(q); got != want {
			t.Errorf("%s: %d rows; want %d", q, got, want)
		}
	}
	mustStatus(t, liveReq(h, key, http.MethodGet, "/v1/live-events?kind=Bad%20Kind", "", ""), http.StatusBadRequest, "bad kind")
}
