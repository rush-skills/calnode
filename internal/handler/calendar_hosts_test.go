package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hosts_event_types drives the admin shell's "connect a calendar" banner: it is
// false for a member who hosts nothing and true once they own or sit on an event
// type. Checked on the unconfigured-calendar path, which is the common case.
func TestCalendarStatus_hostsEventTypes(t *testing.T) {
	h, db, key, uid := setupWorkspaceWithDB(t)

	status := func() bool {
		req := httptest.NewRequest(http.MethodGet, "/v1/calendar/status", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.CalendarStatus)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Hosts bool `json:"hosts_event_types"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
		return resp.Hosts
	}

	if status() {
		t.Fatal("fresh workspace owner hosts nothing yet")
	}
	mustExec(t, db, `INSERT INTO event_types (id, user_id, name, slug, duration_minutes) VALUES ('et1', ?, 'Intro', 'intro', 30)`, uid)
	if !status() {
		t.Fatal("owning an event type should count as hosting")
	}
	mustExec(t, db, `DELETE FROM event_types WHERE id = 'et1'`)
	mustExec(t, db, `INSERT INTO users (id,email,name,iana_timezone) VALUES ('other','o@x.com','O','UTC')`)
	mustExec(t, db, `INSERT INTO event_types (id, user_id, name, slug, duration_minutes) VALUES ('et2', 'other', 'Team', 'team', 30)`)
	mustExec(t, db, `INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority) VALUES ('h1','et2',?,'required',0)`, uid)
	if !status() {
		t.Fatal("a host seat should count as hosting")
	}
}
