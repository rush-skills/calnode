package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/handler"
)

func seriesReq(h *handler.Handler, key, method, id, body string) *httptest.ResponseRecorder {
	path := "/v1/live-series"
	if id != "" {
		path += "/" + id
	}
	req := authReq(method, path, body, key)
	if id != "" {
		req.SetPathValue("id", id)
	}
	var fn http.HandlerFunc
	switch {
	case method == http.MethodPost:
		fn = h.CreateLiveSeries
	case method == http.MethodGet && id == "":
		fn = h.ListLiveSeries
	case method == http.MethodGet:
		fn = h.GetLiveSeries
	case method == http.MethodPatch:
		fn = h.PatchLiveSeries
	case method == http.MethodDelete:
		fn = h.EndLiveSeries
	}
	rec := httptest.NewRecorder()
	h.RequireAuth(fn)(rec, req)
	return rec
}

// The API end to end: create a weekly series, edit one occurrence on its own (recorded
// as an override), see the occurrences in the public feed, end the series.
func TestLiveSeriesAPI_createEditOccurrenceAndEnd(t *testing.T) {
	h, db, key, _ := setupWorkspaceWithDB(t)
	today := time.Now().UTC().Format("2006-01-02")
	rec := seriesReq(h, key, http.MethodPost, "", fmt.Sprintf(`{"title":"Open hours","kind":"office_hours",
		"timezone":"Europe/Sofia","weekdays":[0,1,2,3,4,5,6],"start_time":"23:30","duration_minutes":30,
		"start_date":%q,"join_url":"https://meet.google.com/open-hours","invite_default_participants":false}`, today))
	mustStatus(t, rec, http.StatusCreated, "create series")
	body := liveBody(t, rec)
	series := body["series"].(map[string]any)
	occ := body["occurrences"].([]any)
	if len(occ) < 25 || series["timezone"] != "Europe/Sofia" || series["invite_default_participants"] != false {
		t.Fatalf("series = %v with %d occurrences; want a daily series four weeks deep", series, len(occ))
	}
	first := occ[0].(map[string]any)
	id := first["id"].(string)
	if first["series_id"] != series["id"] || first["join_url"] != "https://meet.google.com/open-hours" || first["invite_default_participants"] != false {
		t.Fatalf("occurrence = %v", first)
	}

	// One occurrence edited alone records what it took over.
	rec = liveReq(h, key, http.MethodPatch, "/v1/live-events/"+id, id, `{"title":"Open hours (holiday edition)","kind":"office_hours"}`)
	mustStatus(t, rec, http.StatusOK, "patch occurrence")
	if ov := liveBody(t, rec)["overrides"].([]any); len(ov) != 1 || ov[0] != "title" {
		t.Errorf("overrides = %v; want [title] (kind was unchanged)", ov)
	}

	// A series edit leaves that title alone and renames the rest.
	rec = seriesReq(h, key, http.MethodPatch, series["id"].(string), `{"title":"Office hours"}`)
	mustStatus(t, rec, http.StatusOK, "patch series")
	for _, o := range liveBody(t, rec)["occurrences"].([]any) {
		m := o.(map[string]any)
		want := "Office hours"
		if m["id"] == id {
			want = "Open hours (holiday edition)"
		}
		if m["title"] != want {
			t.Errorf("occurrence %v title = %q; want %q", m["occurrence_date"], m["title"], want)
		}
	}

	// Feed: occurrences carry their series, and limit widens the upcoming list.
	st := liveStatusBody(t, h, "?limit=20")
	if up := st["upcoming"].([]any); len(up) != 20 || up[0].(map[string]any)["series_id"] != series["id"] {
		t.Errorf("upcoming = %d rows (%v); want 20 with series_id", len(up), up[:1])
	}
	if up := liveStatusBody(t, h, "")["upcoming"].([]any); len(up) != 5 {
		t.Errorf("default upcoming = %d; want 5", len(up))
	}

	rec = seriesReq(h, key, http.MethodDelete, series["id"].(string), "")
	mustStatus(t, rec, http.StatusOK, "end series")
	if b := liveBody(t, rec); b["series"].(map[string]any)["status"] != "ended" || len(b["occurrences"].([]any)) != 0 {
		t.Errorf("after end = %v; want ended with nothing scheduled", b)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM live_events WHERE series_id = ? AND status = 'scheduled'`, series["id"]).Scan(&n)
	if n != 0 {
		t.Errorf("%d occurrences still scheduled after the series ended", n)
	}
}

func TestLiveSeriesAPI_validationAndPermissions(t *testing.T) {
	h, db, adminKey, _ := setupWorkspaceWithDB(t)
	memberKey := seedLiveMember(t, db, "m1", "m1@example.com", false)
	seedLiveMember(t, db, "m2", "m2@example.com", false)
	base := `"title":"S","timezone":"UTC","weekdays":[1],"start_time":"10:00","duration_minutes":60,"start_date":"2030-01-07"`
	for name, body := range map[string]string{
		"missing title":   `{"timezone":"UTC","weekdays":[1],"start_time":"10:00","duration_minutes":60,"start_date":"2030-01-07"}`,
		"bad timezone":    `{` + base + `,"timezone":"Mars/Olympus"}`,
		"bad weekday":     `{` + base + `,"weekdays":[7]}`,
		"no weekday":      `{` + base + `,"weekdays":[]}`,
		"bad time":        `{` + base + `,"start_time":"25:00"}`,
		"end before":      `{` + base + `,"end_date":"2029-12-01"}`,
		"bad skip":        `{` + base + `,"skip_dates":["21 Dec"]}`,
		"backwards range": `{` + base + `,"skip_dates":["2030-12-28/2030-12-21"]}`,
		"bad link":        `{` + base + `,"join_url":"javascript:alert(1)"}`,
	} {
		if rec := seriesReq(h, adminKey, http.MethodPost, "", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d; want 400 — %s", name, rec.Code, rec.Body.String())
		}
	}
	// A member hosts their own series only; an admin may name anyone.
	if rec := seriesReq(h, memberKey, http.MethodPost, "", `{`+base+`,"host_user_ids":["m2"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("member naming another host: %d; want 403", rec.Code)
	}
	rec := seriesReq(h, adminKey, http.MethodPost, "", `{`+base+`,"host_user_ids":["m1","m2"]}`)
	mustStatus(t, rec, http.StatusCreated, "admin rotation")
	id := liveBody(t, rec)["series"].(map[string]any)["id"].(string)
	// m2 hosts it, so may edit it; a member who is neither creator nor host may not.
	seedLiveMember(t, db, "m3", "m3@example.com", false)
	if rec := seriesReq(h, "key-m3", http.MethodPatch, id, `{"title":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("stranger edit: %d; want 403", rec.Code)
	}
	if rec := seriesReq(h, "key-m2", http.MethodPatch, id, `{"title":"Renamed"}`); rec.Code != http.StatusOK {
		t.Errorf("host edit: %d; want 200 — %s", rec.Code, rec.Body.String())
	}
}

// The feed: several kinds at once, and a live row carries its planned window.
func TestLiveStatus_kindListAndLiveWindow(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	end := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, k := range []string{"webinar", "office_hours", "test"} {
		mustStatus(t, liveReq(h, key, http.MethodPost, "/v1/live-events", "",
			fmt.Sprintf(`{"title":"%s","kind":"%s","start_now":true,"scheduled_end_at":%q,"join_url":"https://meet.example.com/%s"}`, k, k, end, k)),
			http.StatusCreated, "create "+k)
	}
	live := liveStatusBody(t, h, "?kind=webinar,office_hours")["live"].([]any)
	if len(live) != 2 {
		t.Fatalf("live = %v; want webinar and office_hours only", live)
	}
	for _, l := range live {
		m := l.(map[string]any)
		if m["kind"] == "test" || m["scheduled_end_at"] != end || m["started_at"] == nil {
			t.Errorf("live row = %v; want its scheduled_end_at", m)
		}
	}
	rec := httptest.NewRecorder()
	h.LiveStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/live/status?kind=Bad!", nil))
	mustStatus(t, rec, http.StatusBadRequest, "bad kind")
}
