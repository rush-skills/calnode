package handler_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/uid"
)

// orgWorkspace is a workspace with three people: the owner who ran setup (an admin),
// a plain member, and a second admin. Each has an API key.
type orgWorkspace struct {
	h         *handler.Handler
	db        *sql.DB
	ownerKey  string
	ownerID   string
	memberKey string
	memberID  string
	adminKey  string
	adminID   string
}

func newOrgWorkspace(t *testing.T) *orgWorkspace {
	t.Helper()
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	ws := &orgWorkspace{h: h, db: database, ownerKey: ownerKey, ownerID: ownerID}

	ws.memberID, ws.memberKey = "u-member", "member-key-"+uid.New()[:8]
	mustExec(t, database, `INSERT INTO users (id, email, name, iana_timezone, is_admin) VALUES (?, 'ana@example.com', 'Ana', 'UTC', 0)`, ws.memberID)
	mustExec(t, database, `INSERT INTO api_keys (id, user_id, name, key_hash, created_at) VALUES ('k-member', ?, 't', ?, '2024-01-01')`,
		ws.memberID, sha256HexForTest(ws.memberKey))

	ws.adminID, ws.adminKey = "u-admin", "admin-key-"+uid.New()[:8]
	mustExec(t, database, `INSERT INTO users (id, email, name, iana_timezone, is_admin) VALUES (?, 'admin@example.com', 'Admin Two', 'UTC', 1)`, ws.adminID)
	mustExec(t, database, `INSERT INTO api_keys (id, user_id, name, key_hash, created_at) VALUES ('k-admin', ?, 't', ?, '2024-01-01')`,
		ws.adminID, sha256HexForTest(ws.adminKey))
	return ws
}

// create makes an event type as the given caller and returns its slug.
func (ws *orgWorkspace) create(t *testing.T, apiKey, visibility string) string {
	t.Helper()
	slug := "et-" + uid.New()[:8]
	body := fmt.Sprintf(`{"slug":%q,"name":"Shared Call","duration_minutes":30}`, slug)
	if visibility != "" {
		body = fmt.Sprintf(`{"slug":%q,"name":"Shared Call","duration_minutes":30,"visibility":%q}`, slug, visibility)
	}
	rec := createET(t, ws.h, apiKey, body)
	return mustString(t, mustCreated(t, rec, "create "+visibility), "slug", "create")
}

type orgETItem struct {
	Slug       string `json:"slug"`
	Owned      bool   `json:"owned"`
	CanEdit    bool   `json:"can_edit"`
	Visibility string `json:"visibility"`
	OwnerID    string `json:"owner_id"`
	OwnerName  string `json:"owner_name"`
}

func (ws *orgWorkspace) list(t *testing.T, apiKey string) map[string]orgETItem {
	t.Helper()
	req := authReq(http.MethodGet, "/v1/event-types", "", apiKey)
	rec := httptest.NewRecorder()
	ws.h.RequireAuth(ws.h.ListEventTypes)(rec, req)
	mustStatus(t, rec, http.StatusOK, "list")
	var resp struct {
		Items []orgETItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	out := map[string]orgETItem{}
	for _, it := range resp.Items {
		out[it.Slug] = it
	}
	return out
}

func (ws *orgWorkspace) get(t *testing.T, apiKey, slug string) (int, orgETItem) {
	t.Helper()
	req := authReq(http.MethodGet, "/v1/event-types/"+slug, "", apiKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	ws.h.RequireAuth(ws.h.GetEventType)(rec, req)
	var it orgETItem
	_ = json.Unmarshal(rec.Body.Bytes(), &it)
	return rec.Code, it
}

func TestOrgEventTypes_listAndGetVisibility(t *testing.T) {
	ws := newOrgWorkspace(t)
	orgSlug := ws.create(t, ws.ownerKey, "")         // default → org
	privSlug := ws.create(t, ws.ownerKey, "private") // owner-only

	// Owner: both, owned and editable.
	owner := ws.list(t, ws.ownerKey)
	for _, slug := range []string{orgSlug, privSlug} {
		it, ok := owner[slug]
		if !ok || !it.Owned || !it.CanEdit {
			t.Errorf("owner list %s: %+v; want present, owned, can_edit", slug, it)
		}
	}
	if owner[orgSlug].Visibility != "org" || owner[privSlug].Visibility != "private" {
		t.Errorf("visibility: org=%q private=%q", owner[orgSlug].Visibility, owner[privSlug].Visibility)
	}

	// Member: the org-wide one, read-only, with the owner named; the private one absent.
	member := ws.list(t, ws.memberKey)
	it, ok := member[orgSlug]
	if !ok || it.Owned || it.CanEdit {
		t.Errorf("member list org: %+v; want present, not owned, not editable", it)
	}
	if it.OwnerID != ws.ownerID || it.OwnerName != "Test Host" {
		t.Errorf("member list org owner: id=%q name=%q; want %q/Test Host", it.OwnerID, it.OwnerName, ws.ownerID)
	}
	if _, ok := member[privSlug]; ok {
		t.Errorf("member list: private event type is visible to a non-owner")
	}

	// Admin: the org-wide one editable; the private one absent (private is owner-only).
	admin := ws.list(t, ws.adminKey)
	if it := admin[orgSlug]; it.Owned || !it.CanEdit {
		t.Errorf("admin list org: %+v; want not owned, can_edit", it)
	}
	if _, ok := admin[privSlug]; ok {
		t.Errorf("admin list: private event type is visible to a non-owner admin")
	}

	// GET mirrors the list.
	if code, it := ws.get(t, ws.memberKey, orgSlug); code != http.StatusOK || it.CanEdit {
		t.Errorf("member get org: %d can_edit=%v; want 200/false", code, it.CanEdit)
	}
	if code, _ := ws.get(t, ws.memberKey, privSlug); code != http.StatusNotFound {
		t.Errorf("member get private: %d; want 404", code)
	}
	if code, it := ws.get(t, ws.adminKey, orgSlug); code != http.StatusOK || !it.CanEdit {
		t.Errorf("admin get org: %d can_edit=%v; want 200/true", code, it.CanEdit)
	}
	if code, _ := ws.get(t, ws.adminKey, privSlug); code != http.StatusNotFound {
		t.Errorf("admin get private: %d; want 404", code)
	}
	if code, it := ws.get(t, ws.ownerKey, privSlug); code != http.StatusOK || !it.Owned || !it.CanEdit {
		t.Errorf("owner get private: %d %+v; want 200, owned, can_edit", code, it)
	}
}

// Any member may create; the result is org-wide by default and owned by its creator,
// so the workspace admin can edit it and the creator keeps it.
func TestOrgEventTypes_memberCreatesOrgWideByDefault(t *testing.T) {
	ws := newOrgWorkspace(t)
	slug := ws.create(t, ws.memberKey, "")

	code, it := ws.get(t, ws.memberKey, slug)
	if code != http.StatusOK || !it.Owned || !it.CanEdit || it.Visibility != "org" || it.OwnerID != ws.memberID {
		t.Fatalf("creator get: %d %+v", code, it)
	}
	if code, it := ws.get(t, ws.ownerKey, slug); code != http.StatusOK || it.Owned || !it.CanEdit {
		t.Errorf("admin get member's event type: %d %+v; want 200, not owned, can_edit", code, it)
	}
	if rec := createET(t, ws.h, ws.memberKey, `{"slug":"bad-vis","name":"X","duration_minutes":30,"visibility":"team"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("create with unknown visibility: %d; want 400", rec.Code)
	}
}

// writeAction performs one mutating call against slug as apiKey and returns the status.
type writeAction struct {
	name string
	do   func(t *testing.T, ws *orgWorkspace, apiKey, slug string) int
}

func doReq(ws *orgWorkspace, fn http.HandlerFunc, method, path, body, apiKey string, pathValues map[string]string) int {
	req := authReq(method, path, body, apiKey)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	ws.h.RequireAuth(fn)(rec, req)
	return rec.Code
}

var writeActions = []writeAction{
	{"update", func(t *testing.T, ws *orgWorkspace, key, slug string) int {
		return doReq(ws, ws.h.PatchEventType, http.MethodPatch, "/v1/event-types/"+slug,
			`{"name":"Renamed"}`, key, map[string]string{"slug": slug})
	}},
	{"hosts", func(t *testing.T, ws *orgWorkspace, key, slug string) int {
		body := fmt.Sprintf(`{"hosts":[{"user_id":%q,"role":"required","priority":0}]}`, ws.ownerID)
		return doReq(ws, ws.h.SetEventTypeHosts, http.MethodPut, "/v1/event-types/"+slug+"/hosts",
			body, key, map[string]string{"slug": slug})
	}},
	{"questions", func(t *testing.T, ws *orgWorkspace, key, slug string) int {
		return doReq(ws, ws.h.CreateQuestion, http.MethodPost, "/v1/event-types/"+slug+"/questions",
			`{"label":"Why?","type":"text"}`, key, map[string]string{"slug": slug})
	}},
	{"duplicate", func(t *testing.T, ws *orgWorkspace, key, slug string) int {
		return doReq(ws, ws.h.DuplicateEventType, http.MethodPost, "/v1/event-types/"+slug+"/duplicate",
			"", key, map[string]string{"slug": slug})
	}},
	{"delete", func(t *testing.T, ws *orgWorkspace, key, slug string) int {
		return doReq(ws, ws.h.DeleteEventType, http.MethodDelete, "/v1/event-types/"+slug,
			"", key, map[string]string{"slug": slug})
	}},
}

// success is what each action returns when allowed.
var writeSuccess = map[string]int{
	"update": http.StatusOK, "hosts": http.StatusOK, "questions": http.StatusCreated,
	"duplicate": http.StatusCreated, "delete": http.StatusNoContent,
}

// The write matrix: owner always; admin on org-wide; nobody else. Every action goes
// through the same helper, so one table covers them all. A fresh event type per cell
// because delete consumes it.
func TestOrgEventTypes_writeMatrix(t *testing.T) {
	ws := newOrgWorkspace(t)
	cells := []struct {
		who, visibility string
		key             string
		want            func(action string) int
	}{
		{"owner", "org", ws.ownerKey, func(a string) int { return writeSuccess[a] }},
		{"owner", "private", ws.ownerKey, func(a string) int { return writeSuccess[a] }},
		{"admin", "org", ws.adminKey, func(a string) int { return writeSuccess[a] }},
		{"admin", "private", ws.adminKey, func(string) int { return http.StatusForbidden }},
		{"member", "org", ws.memberKey, func(string) int { return http.StatusForbidden }},
		{"member", "private", ws.memberKey, func(string) int { return http.StatusForbidden }},
	}
	for _, act := range writeActions {
		for _, c := range cells {
			t.Run(act.name+"/"+c.who+"/"+c.visibility, func(t *testing.T) {
				slug := ws.create(t, ws.ownerKey, c.visibility)
				if got, want := act.do(t, ws, c.key, slug), c.want(act.name); got != want {
					t.Errorf("%s as %s on %s event type: %d; want %d", act.name, c.who, c.visibility, got, want)
				}
			})
		}
		t.Run(act.name+"/unknown-slug", func(t *testing.T) {
			if got := act.do(t, ws, ws.ownerKey, "no-such-slug"); got != http.StatusNotFound {
				t.Errorf("%s on unknown slug: %d; want 404", act.name, got)
			}
		})
	}
}

// Questions update/delete and the admin question list follow the same rule.
func TestOrgEventTypes_questionReadAndWriteScopes(t *testing.T) {
	ws := newOrgWorkspace(t)
	slug := ws.create(t, ws.ownerKey, "")
	qID := createQuestion(t, ws.h, slug, ws.ownerKey, `{"label":"Q","type":"text"}`)

	// Read-only member can list; cannot change.
	if code := doReq(ws, ws.h.ListQuestionsAdmin, http.MethodGet, "/v1/event-types/"+slug+"/questions/admin", "", ws.memberKey,
		map[string]string{"slug": slug}); code != http.StatusOK {
		t.Errorf("member list questions (admin endpoint): %d; want 200", code)
	}
	if code := doReq(ws, ws.h.UpdateQuestion, http.MethodPatch, "/v1/event-types/"+slug+"/questions/"+qID, `{"label":"Hijacked"}`, ws.memberKey,
		map[string]string{"slug": slug, "id": qID}); code != http.StatusForbidden {
		t.Errorf("member update question: %d; want 403", code)
	}
	if code := doReq(ws, ws.h.DeleteQuestion, http.MethodDelete, "/v1/event-types/"+slug+"/questions/"+qID, "", ws.memberKey,
		map[string]string{"slug": slug, "id": qID}); code != http.StatusForbidden {
		t.Errorf("member delete question: %d; want 403", code)
	}
	// Admin can do both on an org-wide event type.
	if code := doReq(ws, ws.h.UpdateQuestion, http.MethodPatch, "/v1/event-types/"+slug+"/questions/"+qID, `{"label":"Edited"}`, ws.adminKey,
		map[string]string{"slug": slug, "id": qID}); code != http.StatusOK {
		t.Errorf("admin update question: %d; want 200", code)
	}
	if code := doReq(ws, ws.h.DeleteQuestion, http.MethodDelete, "/v1/event-types/"+slug+"/questions/"+qID, "", ws.adminKey,
		map[string]string{"slug": slug, "id": qID}); code != http.StatusNoContent {
		t.Errorf("admin delete question: %d; want 204", code)
	}
	// Hosts list is readable by a member on an org-wide event type, not on a private one.
	if code := doReq(ws, ws.h.ListEventTypeHosts, http.MethodGet, "/v1/event-types/"+slug+"/hosts", "", ws.memberKey,
		map[string]string{"slug": slug}); code != http.StatusOK {
		t.Errorf("member list hosts on org: %d; want 200", code)
	}
	priv := ws.create(t, ws.ownerKey, "private")
	if code := doReq(ws, ws.h.ListEventTypeHosts, http.MethodGet, "/v1/event-types/"+priv+"/hosts", "", ws.memberKey,
		map[string]string{"slug": priv}); code != http.StatusNotFound {
		t.Errorf("member list hosts on private: %d; want 404", code)
	}
}

// Visibility is the owner's to change: an admin editing an org-wide event type may
// save everything else, and resubmitting the stored visibility (the editor sends the
// whole form) is not a change.
func TestOrgEventTypes_onlyOwnerChangesVisibility(t *testing.T) {
	ws := newOrgWorkspace(t)
	slug := ws.create(t, ws.ownerKey, "")
	patch := func(key, body string) int {
		return doReq(ws, ws.h.PatchEventType, http.MethodPatch, "/v1/event-types/"+slug, body, key, map[string]string{"slug": slug})
	}
	if code := patch(ws.adminKey, `{"name":"Admin edit","visibility":"org"}`); code != http.StatusOK {
		t.Errorf("admin resubmitting the stored visibility: %d; want 200", code)
	}
	if code := patch(ws.adminKey, `{"visibility":"private"}`); code != http.StatusForbidden {
		t.Errorf("admin hiding someone else's event type: %d; want 403", code)
	}
	if code := patch(ws.ownerKey, `{"visibility":"nope"}`); code != http.StatusBadRequest {
		t.Errorf("owner with an unknown visibility: %d; want 400", code)
	}
	if code := patch(ws.ownerKey, `{"visibility":"private"}`); code != http.StatusOK {
		t.Errorf("owner making it private: %d; want 200", code)
	}
	// Now private: the admin has lost both sight and reach.
	if code, _ := ws.get(t, ws.adminKey, slug); code != http.StatusNotFound {
		t.Errorf("admin get after owner made it private: %d; want 404", code)
	}
	if code := patch(ws.adminKey, `{"name":"x"}`); code != http.StatusForbidden {
		t.Errorf("admin patch after owner made it private: %d; want 403", code)
	}
}

// A duplicate made by an admin keeps the source's owner and visibility: the copy's
// meeting links still mint from that owner's calendar, and being org-wide the admin
// can still edit it.
func TestOrgEventTypes_adminDuplicateKeepsOwner(t *testing.T) {
	ws := newOrgWorkspace(t)
	slug := ws.create(t, ws.ownerKey, "")
	req := authReq(http.MethodPost, "/v1/event-types/"+slug+"/duplicate", "", ws.adminKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	ws.h.RequireAuth(ws.h.DuplicateEventType)(rec, req)
	body := mustCreated(t, rec, "admin duplicate")
	if body["owner_id"] != ws.ownerID || body["owned"] != false || body["can_edit"] != true || body["visibility"] != "org" {
		t.Errorf("copy: owner_id=%v owned=%v can_edit=%v visibility=%v", body["owner_id"], body["owned"], body["can_edit"], body["visibility"])
	}
}

// A row inserted without a visibility (older code, a seeder, direct SQL) is org-wide:
// that is the column default, and the migration applies it to every pre-existing row.
func TestOrgEventTypes_columnDefaultIsOrg(t *testing.T) {
	ws := newOrgWorkspace(t)
	mustExec(t, ws.db, `INSERT INTO event_types (id, user_id, slug, name, duration_minutes) VALUES ('legacy', ?, 'legacy', 'Legacy', 30)`, ws.ownerID)
	if it, ok := ws.list(t, ws.memberKey)["legacy"]; !ok || it.Visibility != "org" {
		t.Errorf("legacy row for a member: present=%v visibility=%q; want org-wide", ok, it.Visibility)
	}
}
