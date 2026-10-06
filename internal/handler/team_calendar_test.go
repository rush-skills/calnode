package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/handler"
)

// eventsCalendar is a fake provider for the external-events pass: it answers
// ListEvents per user and fails for the users listed in failFor.
type eventsCalendar struct {
	calendar.Provider
	name    string
	events  map[string][]calendar.ExternalEvent
	failFor map[string]bool
}

func (p *eventsCalendar) Name() string { return p.name }
func (p *eventsCalendar) ListEvents(_ context.Context, userID string, _, _ time.Time) ([]calendar.ExternalEvent, error) {
	if p.failFor[userID] {
		return nil, context.DeadlineExceeded
	}
	return p.events[userID], nil
}

type teamCalResp struct {
	Members []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"members"`
	Items []struct {
		ID            string `json:"id"`
		Kind          string `json:"kind"`
		MemberID      string `json:"member_id"`
		Title         string `json:"title"`
		Start         string `json:"start"`
		End           string `json:"end"`
		AllDay        bool   `json:"all_day"`
		Location      string `json:"location"`
		BookingID     string `json:"booking_id"`
		EventTypeName string `json:"event_type_name"`
		AttendeeName  string `json:"attendee_name"`
		Status        string `json:"status"`
		Source        string `json:"source"`
	} `json:"items"`
}

func teamCalGet(t *testing.T, h *handler.Handler, query, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/team-calendar?"+query, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	h.TeamCalendar(rec, req)
	return rec
}

func teamCalDecode(t *testing.T, rec *httptest.ResponseRecorder) teamCalResp {
	t.Helper()
	mustStatus(t, rec, http.StatusOK, "team calendar")
	var out teamCalResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	return out
}

// seedMemberKey adds an active non-admin member with an API key and returns the key.
func seedMemberKey(t *testing.T, database *sql.DB, id, email string) string {
	t.Helper()
	seedMember(t, database, id, email)
	key := "member-key-" + id
	mustExec(t, database, `INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES (?,?,'t',?,'2024-01-01')`,
		"k-"+id, id, sha256HexForTest(key))
	return key
}

func createShare(t *testing.T, h *handler.Handler, apiKey, name string) (id, token string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateTeamCalendarShare)(rec, authReq(http.MethodPost, "/v1/team-calendar/shares", `{"name":"`+name+`"}`, apiKey))
	body := mustCreated(t, rec, "create share")
	return mustString(t, body, "id", "create share"), mustString(t, body, "token", "create share")
}

func TestTeamCalendar_rangeValidation(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	for _, tc := range []struct{ name, query string }{
		{"missing", ""},
		{"missing to", "from=2026-06-01"},
		{"malformed", "from=2026-06-01&to=junk"},
		{"reversed", "from=2026-06-10&to=2026-06-01"},
		{"over 42 days", "from=2026-06-01&to=2026-07-14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustStatus(t, teamCalGet(t, h, tc.query, key), http.StatusBadRequest, tc.name)
		})
	}
	// Exactly 42 days apart is the month grid, and must pass.
	mustStatus(t, teamCalGet(t, h, "from=2026-06-01&to=2026-07-13", key), http.StatusOK, "42 days")
}

func TestTeamCalendar_requiresSessionOrToken(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	q := "from=2026-06-01&to=2026-06-07"

	mustStatus(t, teamCalGet(t, h, q, ""), http.StatusUnauthorized, "no auth")
	mustStatus(t, teamCalGet(t, h, q+"&token=tcs_nope", ""), http.StatusUnauthorized, "unknown token")
	// A bad token never falls through to the session/API-key path.
	mustStatus(t, teamCalGet(t, h, q+"&token=tcs_nope", adminKey), http.StatusUnauthorized, "bad token with key")

	// Any member, not only admins, can read the calendar when signed in.
	memberKey := seedMemberKey(t, database, "u2", "m@example.com")
	mustStatus(t, teamCalGet(t, h, q, memberKey), http.StatusOK, "member")

	id, token := createShare(t, h, adminKey, "Lobby screen")
	mustStatus(t, teamCalGet(t, h, q+"&token="+token, ""), http.StatusOK, "valid token")

	rec := httptest.NewRecorder()
	req := authReq(http.MethodDelete, "/v1/team-calendar/shares/"+id, "", adminKey)
	req.SetPathValue("id", id)
	h.RequireAuth(h.RevokeTeamCalendarShare)(rec, req)
	mustStatus(t, rec, http.StatusNoContent, "revoke")
	mustStatus(t, teamCalGet(t, h, q+"&token="+token, ""), http.StatusUnauthorized, "revoked token")
}

func TestTeamCalendar_bookingItemsPerHostSeat(t *testing.T) {
	h, database, key, ownerID := setupWorkspaceWithDB(t)
	_, etID := seedEventTypeHTTP(t, h, key)
	seedMember(t, database, "u2", "two@example.com")
	seedMember(t, database, "u3", "three@example.com")
	mustExec(t, database, `UPDATE users SET archived_at = '2026-01-01T00:00:00Z' WHERE id = 'u3'`)

	// One booking with three seats (owner primary, u2, archived u3); one legacy booking
	// with no booking_hosts rows; one cancelled; one outside the range.
	mustExec(t, database, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status,meeting_link)
		VALUES ('b1',?,?,'2026-06-03T10:00:00Z','2026-06-03T10:30:00Z','confirmed','https://meet.example/x')`, etID, ownerID)
	mustExec(t, database, `INSERT INTO booking_attendees (id,booking_id,name,email,is_organizer) VALUES ('a1','b1','Ada Lovelace','ada@example.com',1)`)
	mustExec(t, database, `INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id) VALUES ('bh1','b1',?,1,'gcal-owner-1')`, ownerID)
	mustExec(t, database, `INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh2','b1','u2',0)`)
	mustExec(t, database, `INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh3','b1','u3',0)`)
	mustExec(t, database, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b-legacy',?,'u2','2026-06-04T09:00:00Z','2026-06-04T09:30:00Z','confirmed')`, etID)
	mustExec(t, database, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b-cancelled',?,?,'2026-06-04T11:00:00Z','2026-06-04T11:30:00Z','cancelled')`, etID, ownerID)
	mustExec(t, database, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b-far',?,?,'2026-08-04T11:00:00Z','2026-08-04T11:30:00Z','confirmed')`, etID, ownerID)

	out := teamCalDecode(t, teamCalGet(t, h, "from=2026-06-01&to=2026-06-07", key))

	if len(out.Members) != 2 {
		t.Fatalf("members = %d; want 2 active (archived u3 excluded): %+v", len(out.Members), out.Members)
	}
	if out.Members[0].ID != ownerID || out.Members[0].Color == "" || out.Members[1].Color == out.Members[0].Color {
		t.Errorf("members must be oldest-first with distinct palette colours: %+v", out.Members)
	}

	seats := map[string]int{}
	for _, it := range out.Items {
		if it.Kind != "booking" || it.Source != "calnode" || it.Status != "confirmed" {
			t.Errorf("unexpected item %+v", it)
		}
		seats[it.BookingID+"/"+it.MemberID]++
	}
	for _, want := range []string{"b1/" + ownerID, "b1/u2", "b-legacy/u2"} {
		if seats[want] != 1 {
			t.Errorf("seat %s: got %d items; want 1 (all: %v)", want, seats[want], seats)
		}
	}
	if len(out.Items) != 3 {
		t.Errorf("items = %d; want 3 (no archived seat, no cancelled, nothing out of range): %+v", len(out.Items), out.Items)
	}
	for _, it := range out.Items {
		if it.BookingID == "b1" {
			if it.Title != "Test Meeting · Ada Lovelace" || it.AttendeeName != "Ada Lovelace" || it.EventTypeName != "Test Meeting" {
				t.Errorf("b1 details: %+v", it)
			}
			if it.Location != "https://meet.example/x" || it.Start != "2026-06-03T10:00:00Z" || it.End != "2026-06-03T10:30:00Z" {
				t.Errorf("b1 location/times: %+v", it)
			}
		}
	}
}

func TestTeamCalendar_externalEventsMergedAndDeduplicated(t *testing.T) {
	h, database, key, ownerID := setupWorkspaceWithDB(t)
	_, etID := seedEventTypeHTTP(t, h, key)
	seedMember(t, database, "u2", "two@example.com")
	seedMember(t, database, "u3", "three@example.com")
	for _, u := range []string{ownerID, "u2", "u3"} {
		mustExec(t, database, `INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,created_at)
			VALUES (?,?,'google','enc','primary','2026-01-01T00:00:00Z')`, "cc-"+u, u)
	}
	mustExec(t, database, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1',?,?,'2026-06-03T10:00:00Z','2026-06-03T10:30:00Z','confirmed')`, etID, ownerID)
	mustExec(t, database, `INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id) VALUES ('bh1','b1',?,1,'ev-ours')`, ownerID)

	day := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	fake := &eventsCalendar{
		name: "google",
		events: map[string][]calendar.ExternalEvent{
			ownerID: {
				{ID: "ev-ours", Title: "Test Meeting", Start: day.Add(10 * time.Hour), End: day.Add(10*time.Hour + 30*time.Minute)}, // our own event: dropped
				{ID: "ev-dentist", Title: "Dentist", Location: "Main St", Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour)},
				{ID: "ev-dentist", Title: "Dentist", Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour)}, // same event from a second selected calendar
			},
			"u2": {
				{ID: "ev-offsite", Title: "", AllDay: true, Start: day, End: day.Add(24 * time.Hour)},
			},
		},
		failFor: map[string]bool{"u3": true},
	}
	// The owner also has a Microsoft account whose provider fails: the Google events
	// above must still come through (partial by design), not vanish with the error.
	mustExec(t, database, `INSERT INTO calendar_connections (id,user_id,provider,access_token_enc,calendar_id,created_at)
		VALUES ('cc-ms',?,'microsoft','enc','primary','2026-01-01T00:00:00Z')`, ownerID)
	broken := &eventsCalendar{name: "microsoft", failFor: map[string]bool{ownerID: true}}
	svc := calendar.NewService(database)
	svc.Register(fake)
	svc.Register(broken)
	h.SetCalendar(svc)

	out := teamCalDecode(t, teamCalGet(t, h, "from=2026-06-01&to=2026-06-07", key))

	var kinds []string
	for _, it := range out.Items {
		kinds = append(kinds, it.Kind+":"+it.MemberID+":"+it.Title)
	}
	got := strings.Join(kinds, ",")
	want := "external:u2:Busy,booking:" + ownerID + ":Test Meeting,external:" + ownerID + ":Dentist"
	if got != want {
		t.Fatalf("items:\n got %s\nwant %s", got, want)
	}
	for _, it := range out.Items {
		if it.Kind != "external" {
			continue
		}
		if it.Source != "google" || it.Status != "confirmed" || it.BookingID != "" {
			t.Errorf("external item shape: %+v", it)
		}
		if it.MemberID == "u2" && !it.AllDay {
			t.Errorf("all-day flag lost: %+v", it)
		}
		if it.MemberID == ownerID && it.Location != "Main St" {
			t.Errorf("location lost: %+v", it)
		}
	}
	// u3's provider failure was logged and skipped; the response still succeeded with
	// everyone else's events, which the assertions above already proved.
}

func TestTeamCalendarShares_adminOnly(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	memberKey := seedMemberKey(t, database, "u2", "m@example.com")

	// Member: forbidden on every share endpoint.
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateTeamCalendarShare)(rec, authReq(http.MethodPost, "/v1/team-calendar/shares", `{"name":"x"}`, memberKey))
	mustStatus(t, rec, http.StatusForbidden, "member create")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.ListTeamCalendarShares)(rec, authReq(http.MethodGet, "/v1/team-calendar/shares", "", memberKey))
	mustStatus(t, rec, http.StatusForbidden, "member list")

	// Admin: create returns the token once; list never does; revoke flips revoked_at.
	rec = httptest.NewRecorder()
	h.RequireAuth(h.CreateTeamCalendarShare)(rec, authReq(http.MethodPost, "/v1/team-calendar/shares", `{"name":"  "}`, adminKey))
	mustStatus(t, rec, http.StatusBadRequest, "blank name")

	id, token := createShare(t, h, adminKey, "Lobby")
	if !strings.HasPrefix(token, "tcs_") || len(token) != 4+64 {
		t.Errorf("token shape: %q", token)
	}
	var stored string
	if err := database.QueryRow(`SELECT token_hash FROM team_calendar_shares WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != sha256HexForTest(token) {
		t.Errorf("stored hash must be the SHA-256 of the token, never the token")
	}

	rec = httptest.NewRecorder()
	req := authReq(http.MethodDelete, "/v1/team-calendar/shares/"+id, "", memberKey)
	req.SetPathValue("id", id)
	h.RequireAuth(h.RevokeTeamCalendarShare)(rec, req)
	mustStatus(t, rec, http.StatusForbidden, "member revoke")

	rec = httptest.NewRecorder()
	h.RequireAuth(h.ListTeamCalendarShares)(rec, authReq(http.MethodGet, "/v1/team-calendar/shares", "", adminKey))
	body := mustJSON(t, rec, http.StatusOK, "admin list")
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), stored) {
		t.Errorf("list must not expose tokens or hashes: %s", rec.Body.String())
	}
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["revoked_at"] != nil {
		t.Fatalf("list: %v", body)
	}

	rec = httptest.NewRecorder()
	req = authReq(http.MethodDelete, "/v1/team-calendar/shares/"+id, "", adminKey)
	req.SetPathValue("id", id)
	h.RequireAuth(h.RevokeTeamCalendarShare)(rec, req)
	mustStatus(t, rec, http.StatusNoContent, "admin revoke")

	rec = httptest.NewRecorder()
	req = authReq(http.MethodDelete, "/v1/team-calendar/shares/"+id, "", adminKey)
	req.SetPathValue("id", id)
	h.RequireAuth(h.RevokeTeamCalendarShare)(rec, req)
	mustStatus(t, rec, http.StatusNotFound, "revoke twice")

	rec = httptest.NewRecorder()
	h.RequireAuth(h.ListTeamCalendarShares)(rec, authReq(http.MethodGet, "/v1/team-calendar/shares", "", adminKey))
	body = mustJSON(t, rec, http.StatusOK, "admin list after revoke")
	if body["items"].([]any)[0].(map[string]any)["revoked_at"] == nil {
		t.Errorf("revoked share must stay listed with revoked_at set: %v", body)
	}
}

func TestTeamCalendarEmbed_frameableAndTokenGated(t *testing.T) {
	h, _, adminKey, _ := setupWorkspaceWithDB(t)
	_, token := createShare(t, h, adminKey, "Dashboard")

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.TeamCalendarEmbed(rec, httptest.NewRequest(http.MethodGet, "/embed/team-calendar"+query, nil))
		return rec
	}

	rec := get("?token=" + token)
	mustStatus(t, rec, http.StatusOK, "embed page")
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("embed page must not send X-Frame-Options (it would block the iframe); got %q", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors *") || strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("embed page must allow any frame ancestor; CSP = %q", csp)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", got)
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Errorf("the page must carry the token for its own fetches")
	}
	if !strings.Contains(rec.Body.String(), "/v1/team-calendar?") {
		t.Errorf("the page must fetch the data endpoint")
	}

	for _, tc := range []struct{ name, query string }{
		{"missing", ""},
		{"unknown", "?token=tcs_unknown"},
	} {
		rec := get(tc.query)
		mustStatus(t, rec, http.StatusNotFound, "embed "+tc.name)
		if strings.Contains(rec.Body.String(), "tcs_") {
			t.Errorf("%s: 404 page leaked a token", tc.name)
		}
	}
}
