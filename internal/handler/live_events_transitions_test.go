package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/slots"
)

// userCalendar is a Google-shaped fake that records WHICH user each create/cancel ran as,
// and can hold CreateEvent open so a transition can be raced against it.
type userCalendar struct {
	calendar.Provider
	mu      sync.Mutex
	n       int
	creates []string // "<user>:meet|nomeet:loc=<location>"
	cancels []string // "<user>:<event id>"
	block   chan struct{}
	entered chan struct{}
}

func (p *userCalendar) Name() string                                         { return "google" }
func (p *userCalendar) InvitesGuests() bool                                  { return true }
func (p *userCalendar) HasDestination(context.Context, string) (bool, error) { return true, nil }
func (p *userCalendar) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}
func (p *userCalendar) CreateEvent(_ context.Context, uid string, in calendar.CreateEventParams) (string, string, string, error) {
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	m, link := "nomeet", ""
	if in.AddMeet {
		m, link = "meet", "https://meet.google.com/"+uid+"-"+string(rune('0'+p.n))
	}
	p.creates = append(p.creates, uid+":"+m+":loc="+in.Location)
	return "evt-" + uid + "-" + string(rune('0'+p.n)), link, "primary", nil
}
func (p *userCalendar) UpdateEvent(context.Context, string, string, string, time.Time, time.Time, string) error {
	return nil
}
func (p *userCalendar) CancelEvent(_ context.Context, uid, _, eventID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, uid+":"+eventID)
	return nil
}
func (p *userCalendar) snap() (creates, cancels []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.creates...), append([]string(nil), p.cancels...)
}

func installUserCalendar(t *testing.T, h *handler.Handler, dbx *sql.DB, p *userCalendar, users ...string) {
	t.Helper()
	for _, u := range users {
		mustExec(t, dbx, `INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,is_destination) VALUES (?,?,'google','test','primary',1)`, "conn-"+u, u)
	}
	svc := calendar.NewService(dbx)
	svc.Register(p)
	h.SetCalendar(svc)
}

// M2: a host change cancels the calendar event as the OLD host (it lives on their
// calendar), drops the minted link and mints a fresh Meet for the new host.
func TestLiveEvents_hostChangeCancelsAsOldHostAndRemints(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	seedLiveMember(t, db, "bob", "bob@example.com", false)
	p := &userCalendar{}
	installUserCalendar(t, h, db, p, ownerID, "bob")

	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"OH","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	created := liveBody(t, rec)
	id := created["id"].(string)
	if created["join_url_minted"] != true || !strings.HasPrefix(created["join_url"].(string), "https://meet.google.com/"+ownerID) {
		t.Fatalf("created = %v; want a minted link for the owner", created)
	}

	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"host_user_id":"bob"}`)
	mustStatus(t, rec, http.StatusOK, "patch host")
	after := liveBody(t, rec)
	creates, cancels := p.snap()
	if len(cancels) != 1 || cancels[0] != ownerID+":evt-"+ownerID+"-1" {
		t.Errorf("cancels = %v; want the owner's event cancelled as the owner", cancels)
	}
	if len(creates) != 2 || creates[1] != "bob:meet:loc=" {
		t.Errorf("creates = %v; want a fresh Meet minted for bob", creates)
	}
	if got := after["join_url"].(string); !strings.HasPrefix(got, "https://meet.google.com/bob") || after["join_url_minted"] != true {
		t.Errorf("join_url after host change = %q minted=%v; want bob's fresh Meet", got, after["join_url_minted"])
	}
	if after["host_user_id"] != "bob" || after["has_calendar_event"] != true {
		t.Errorf("after = %v", after)
	}
	var minted int
	db.QueryRow(`SELECT join_url_minted FROM live_events WHERE id = ?`, id).Scan(&minted)
	if minted != 1 {
		t.Errorf("join_url_minted stored = %d; want 1", minted)
	}
}

// M2: a manual join_url is the operator's and survives a host change: the new host's
// event carries it as location and nothing is minted.
func TestLiveEvents_manualLinkSurvivesHostChange(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	seedLiveMember(t, db, "bob", "bob@example.com", false)
	p := &userCalendar{}
	installUserCalendar(t, h, db, p, ownerID, "bob")

	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"OH","join_url":"https://x.example/manual","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	id := liveBody(t, rec)["id"].(string)
	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"host_user_id":"bob"}`)
	mustStatus(t, rec, http.StatusOK, "patch host")
	after := liveBody(t, rec)
	if after["join_url"] != "https://x.example/manual" || after["join_url_minted"] != false {
		t.Errorf("manual link not kept: %v", after)
	}
	creates, cancels := p.snap()
	if len(creates) != 2 || creates[1] != "bob:nomeet:loc=https://x.example/manual" {
		t.Errorf("creates = %v; want bob's event carrying the manual link, no mint", creates)
	}
	if len(cancels) != 1 || !strings.HasPrefix(cancels[0], ownerID+":") {
		t.Errorf("cancels = %v; want the owner's event cancelled as the owner", cancels)
	}

	// Replacing a minted link by hand marks it manual, so a later host change keeps it.
	rec = liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"Minted","scheduled_start_at":"2030-01-02T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create minted")
	id2 := liveBody(t, rec)["id"].(string)
	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id2, id2, `{"join_url":"https://x.example/typed"}`)
	mustStatus(t, rec, http.StatusOK, "patch link")
	if b := liveBody(t, rec); b["join_url_minted"] != false {
		t.Errorf("typed link still flagged minted: %v", b)
	}
	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id2, id2, `{"host_user_id":"bob"}`)
	mustStatus(t, rec, http.StatusOK, "patch host 2")
	if b := liveBody(t, rec); b["join_url"] != "https://x.example/typed" {
		t.Errorf("typed link lost on host change: %v", b)
	}
}

// M3: the sweep never auto-starts a stale row - one whose scheduled end has passed, or an
// open-ended one scheduled more than liveEventMaxRunning ago. Those get auto_start=0; a
// recent open-ended one still starts.
func TestSweepLiveEvents_staleRowsNeverStart(t *testing.T) {
	h, db, key, _ := setupWorkspaceWithDB(t)
	create := func(title, extra string) string {
		rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"`+title+`","join_url":"https://x.example/`+title+`"`+extra+`}`)
		mustStatus(t, rec, http.StatusCreated, "create "+title)
		return liveBody(t, rec)["id"].(string)
	}
	ts := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	oldOpen := create("old-open", `,"scheduled_start_at":"`+ts(-72*time.Hour)+`"`)
	oldClosed := create("old-closed", `,"scheduled_start_at":"`+ts(-72*time.Hour)+`","scheduled_end_at":"`+ts(-71*time.Hour)+`"`)
	endPassed := create("end-passed", `,"scheduled_start_at":"`+ts(-2*time.Hour)+`","scheduled_end_at":"`+ts(-time.Hour)+`"`)
	recent := create("recent", `,"scheduled_start_at":"`+ts(-time.Minute)+`"`)
	stillRunning := create("running", `,"scheduled_start_at":"`+ts(-3*time.Hour)+`","scheduled_end_at":"`+ts(time.Hour)+`"`)

	h.SweepLiveEvents(context.Background(), time.Now())

	state := func(id string) (string, int) {
		var s string
		var a int
		if err := db.QueryRow(`SELECT status, auto_start FROM live_events WHERE id = ?`, id).Scan(&s, &a); err != nil {
			t.Fatal(err)
		}
		return s, a
	}
	for _, id := range []string{oldOpen, oldClosed, endPassed} {
		if s, a := state(id); s != "scheduled" || a != 0 {
			t.Errorf("stale %s: status=%s auto_start=%d; want scheduled with auto_start off", id, s, a)
		}
	}
	for _, id := range []string{recent, stillRunning} {
		if s, _ := state(id); s != "live" {
			t.Errorf("%s: status=%s; want live", id, s)
		}
	}
	if live := liveStatusBody(t, h, "")["live"].([]any); len(live) != 2 {
		t.Errorf("public feed shows %d live; want 2", len(live))
	}
}

// M1: the sweep is in the provider minting the Meet when a manual start arrives. The
// start waits for the claim, finds the row live and returns it: one calendar event.
func TestLiveEvents_concurrentStartMintsOnce(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	start := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"OH","scheduled_start_at":"`+start+`"}`)
	mustStatus(t, rec, http.StatusCreated, "create") // no calendar yet: nothing minted
	id := liveBody(t, rec)["id"].(string)
	p := &userCalendar{block: make(chan struct{}), entered: make(chan struct{}, 4)}
	installUserCalendar(t, h, db, p, ownerID)

	sweepDone := make(chan struct{})
	go func() { h.SweepLiveEvents(context.Background(), time.Now()); close(sweepDone) }()
	<-p.entered
	startCode := make(chan int, 1)
	go func() {
		r := liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, "")
		startCode <- r.Code
	}()
	select {
	case <-p.entered:
		t.Fatal("the manual start reached the provider while the sweep's mint was in flight: two events")
	case <-time.After(150 * time.Millisecond):
	}
	close(p.block)
	<-sweepDone
	if code := <-startCode; code != http.StatusOK {
		t.Errorf("manual start after the sweep won -> %d; want 200 (idempotent)", code)
	}
	creates, cancels := p.snap()
	if len(creates) != 1 || len(cancels) != 0 {
		t.Errorf("creates=%v cancels=%v; want exactly one event and no cancel", creates, cancels)
	}
	ev := liveBody(t, liveReq(h, key, http.MethodGet, "/v1/live-events/"+id, id, ""))
	if ev["status"] != "live" {
		t.Errorf("status = %v; want live", ev["status"])
	}
}

// M1: DELETE wins. A cancel that lands while the sweep is minting leaves the row cancelled,
// the just-minted event cancelled again, nothing on the public feed, and a later start 409.
func TestLiveEvents_cancelDuringMintWins(t *testing.T) {
	h, db, key, ownerID := setupWorkspaceWithDB(t)
	start := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	rec := liveReq(h, key, http.MethodPost, "/v1/live-events", "", `{"title":"OH","scheduled_start_at":"`+start+`"}`)
	mustStatus(t, rec, http.StatusCreated, "create")
	id := liveBody(t, rec)["id"].(string)
	p := &userCalendar{block: make(chan struct{}), entered: make(chan struct{}, 4)}
	installUserCalendar(t, h, db, p, ownerID)

	sweepDone := make(chan struct{})
	go func() { h.SweepLiveEvents(context.Background(), time.Now()); close(sweepDone) }()
	<-p.entered
	mustStatus(t, liveReq(h, key, http.MethodDelete, "/v1/live-events/"+id, id, ""), http.StatusOK, "cancel during mint")
	close(p.block)
	<-sweepDone

	ev := liveBody(t, liveReq(h, key, http.MethodGet, "/v1/live-events/"+id, id, ""))
	if ev["status"] != "cancelled" || ev["has_calendar_event"] != false || ev["join_url"] != "" {
		t.Errorf("after cancel+sweep: %v; want cancelled, no calendar event, no link", ev)
	}
	creates, cancels := p.snap()
	if len(creates) != 1 || len(cancels) != 1 || cancels[0] != ownerID+":evt-"+ownerID+"-1" {
		t.Errorf("creates=%v cancels=%v; want the minted event cancelled again", creates, cancels)
	}
	if live := liveStatusBody(t, h, "")["live"].([]any); len(live) != 0 {
		t.Errorf("public feed shows a cancelled session: %v", live)
	}
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/start", id, ""), http.StatusConflict, "start after cancel")
	mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events/"+id+"/end", id, ""), http.StatusConflict, "end after cancel")
	// A later sweep must not resurrect it either.
	h.SweepLiveEvents(context.Background(), time.Now().Add(time.Hour))
	if s := liveBody(t, liveReq(h, key, http.MethodGet, "/v1/live-events/"+id, id, ""))["status"]; s != "cancelled" {
		t.Errorf("sweep resurrected a cancelled session: %v", s)
	}
}
