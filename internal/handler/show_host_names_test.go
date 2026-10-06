package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/handler"
)

// The "Show host names on booking pages" switch (Settings → Branding, default ON) and the
// removal of the "Powered by Calnode" attribution — see docs/features/public-pages.md.

func getBranding(t *testing.T, h *handler.Handler, apiKey string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetBranding)(rec, authReq(http.MethodGet, "/v1/settings/branding", "", apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("get branding: %d — %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode branding: %v", err)
	}
	return resp
}

func patchBranding(t *testing.T, h *handler.Handler, apiKey, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchBranding)(rec, authReq(http.MethodPatch, "/v1/settings/branding", body, apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch branding %s: %d — %s", body, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode branding: %v", err)
	}
	return resp
}

func setShowHostNames(t *testing.T, h *handler.Handler, apiKey string, on bool) {
	t.Helper()
	patchBranding(t, h, apiKey, fmt.Sprintf(`{"show_host_names":%t}`, on))
}

func TestBrandingSettings_showHostNames_roundTrip(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)

	if got := getBranding(t, h, apiKey)["show_host_names"]; got != true {
		t.Fatalf("show_host_names default = %v; want true (upstream behaviour unchanged)", got)
	}
	if got := patchBranding(t, h, apiKey, `{"show_host_names":false}`)["show_host_names"]; got != false {
		t.Fatalf("show_host_names after patch false = %v; want false", got)
	}
	// An omitted key keeps the stored value — the pointer field is what distinguishes
	// "not sent" from "sent as false".
	if got := patchBranding(t, h, apiKey, `{"business_name":"Acme"}`)["show_host_names"]; got != false {
		t.Errorf("show_host_names after a patch that omits it = %v; want the stored false", got)
	}
	if got := getBranding(t, h, apiKey)["show_host_names"]; got != false {
		t.Errorf("show_host_names persisted = %v; want false", got)
	}
	if got := patchBranding(t, h, apiKey, `{"show_host_names":true}`)["show_host_names"]; got != true {
		t.Errorf("show_host_names after patch true = %v; want true", got)
	}
}

func renderBookPage(t *testing.T, h *handler.Handler, slug string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/book/"+slug, nil)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.BookPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("book page: %d — %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestBookPage_showHostNamesOff: the rendered page carries the event name but never the
// host's name — not in the face stack, the host line, the "%s has no available times"
// subject (SOLE_HOST) or anywhere else — and drops the host block entirely rather than
// leaving an empty row above the title.
func TestBookPage_showHostNamesOff(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t) // owner is "Test User"
	slug, _ := seedEventTypeHTTP(t, h, apiKey)

	body := renderBookPage(t, h, slug)
	if !strings.Contains(body, "Test User") {
		t.Fatalf("with the default (on), the page should name the host; body: %.800s", body)
	}
	if !strings.Contains(body, `id="host-faces"`) || !strings.Contains(body, `id="host-name"`) {
		t.Error("with the default (on), the page should render the host face/name block")
	}

	setShowHostNames(t, h, apiKey, false)
	body = renderBookPage(t, h, slug)
	if strings.Contains(body, "Test User") {
		t.Errorf("host name leaked into the booking page with show_host_names off:\n%s", excerptAround(body, "Test User"))
	}
	if !strings.Contains(body, "Test Meeting") {
		t.Error("event name missing from the booking page with show_host_names off")
	}
	if strings.Contains(body, `id="host-faces"`) || strings.Contains(body, `id="host-name"`) {
		t.Error("host face/name block should be omitted (not just emptied) with show_host_names off")
	}
}

func TestPublicEventType_showHostNamesOff(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)

	fetch := func() (string, []any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/event-types/"+slug+"/public", nil)
		req.SetPathValue("slug", slug)
		rec := httptest.NewRecorder()
		h.PublicEventType(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("public: %d — %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Hosts []any `json:"hosts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rec.Body.String(), resp.Hosts
	}

	if body, hosts := fetch(); len(hosts) != 1 || !strings.Contains(body, "Test User") {
		t.Fatalf("with the default (on), hosts = %v; want the owner", hosts)
	}

	setShowHostNames(t, h, apiKey, false)
	body, hosts := fetch()
	if len(hosts) != 0 {
		t.Errorf("hosts = %v; want an empty list with show_host_names off", hosts)
	}
	if strings.Contains(body, "Test User") {
		t.Errorf("host name leaked into the public event-type JSON: %s", body)
	}
	if !strings.Contains(body, `"name":"Test Meeting"`) {
		t.Errorf("event name missing from the public event-type JSON: %s", body)
	}
}

// TestGetSlots_showHostNamesOff: the id → name/avatar map the pages use to narrow the
// header to a picked slot's host is sent empty, while the slots keep their opaque ids.
func TestGetSlots_showHostNamesOff(t *testing.T) {
	h, apiKey, userID := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey) // wide-open availability

	day := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	fetch := func() (string, map[string]map[string]string, int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/v1/event-types/"+slug+"/slots?from="+day+"&to="+day+"&tz=UTC", nil)
		req.SetPathValue("slug", slug)
		rec := httptest.NewRecorder()
		h.GetSlots(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("slots: %d — %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Slots []slotResp                   `json:"slots"`
			Hosts map[string]map[string]string `json:"hosts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rec.Body.String(), resp.Hosts, len(resp.Slots)
	}

	if _, hosts, n := fetch(); n == 0 || hosts[userID]["name"] != "Test User" {
		t.Fatalf("with the default (on): %d slots, hosts = %v; want slots and the owner's name", n, hosts)
	}

	setShowHostNames(t, h, apiKey, false)
	body, hosts, n := fetch()
	if n == 0 {
		t.Error("slots disappeared with show_host_names off; the setting is presentation-only")
	}
	if len(hosts) != 0 {
		t.Errorf("hosts = %v; want an empty map with show_host_names off", hosts)
	}
	if strings.Contains(body, "Test User") {
		t.Errorf("host name leaked into the slots response: %s", body)
	}
}

// TestCreateBooking_showHostNamesOff: the public create response is what book.html uses
// to show the assigned host on the confirmation screen, so it is withheld too.
func TestCreateBooking_showHostNamesOff(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	setShowHostNames(t, h, apiKey, false)

	start := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour).Add(10 * time.Hour)
	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":%q,"name":"Test Attendee","email":"attendee@example.com","timezone":"UTC"}`,
		slug, start.Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create booking: %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    string `json:"id"`
		Hosts []any  `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("booking id missing: the setting must not affect booking itself")
	}
	if len(resp.Hosts) != 0 {
		t.Errorf("hosts = %v; want none on the public create response with show_host_names off", resp.Hosts)
	}
	if strings.Contains(rec.Body.String(), "Test User") {
		t.Errorf("host name leaked into the create-booking response: %s", rec.Body.String())
	}
}

func TestManagePage_showHostNamesOff(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t) // owner is "Test Host"
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	start := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour).Add(9 * time.Hour)
	bookingID := createBookingViaHTTP(t, h, slug, start.Format(time.RFC3339))
	tok := issueTestToken(t, database, bookingID)

	render := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/manage/"+tok, nil)
		req.SetPathValue("token", tok)
		rec := httptest.NewRecorder()
		h.ManagePage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("manage page: %d — %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	if body := render(); !strings.Contains(body, "Test Host") {
		t.Fatalf("with the default (on), the manage page should name the host; body: %.800s", body)
	}

	setShowHostNames(t, h, apiKey, false)
	body := render()
	if strings.Contains(body, "Test Host") {
		t.Errorf("host name leaked into the manage page with show_host_names off:\n%s", excerptAround(body, "Test Host"))
	}
	if !strings.Contains(body, "Test Meeting") {
		t.Error("event name missing from the manage page with show_host_names off")
	}
	if strings.Contains(body, `class="host-name"`) || strings.Contains(body, `class="avatar-initials"`) {
		t.Error("host avatar/name block should be omitted with show_host_names off")
	}
}

// TestTeamPage_showHostNamesOff: the team roster is a list of host names, so it goes;
// the person page is about one named person and keeps its name.
func TestTeamPage_showHostNamesOff(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,handle) VALUES ('u2','miri@example.com','Miri Tane','UTC',0,'miri')`) //nolint:errcheck
	database.Exec(`INSERT INTO teams (id,name,slug) VALUES ('t1','Design','design')`)                                                            //nolint:errcheck
	database.Exec(`INSERT INTO team_members (id,team_id,user_id) VALUES ('tm1','t1','u2')`)                                                      //nolint:errcheck
	slug, etID := seedEventTypeHTTP(t, h, apiKey)
	database.Exec(`UPDATE event_types SET team_id = 't1' WHERE id = ?`, etID) //nolint:errcheck

	team := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/team/design", nil)
		req.SetPathValue("slug", "design")
		h.TeamPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("team page: %d — %.200s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	if body := team(); !strings.Contains(body, "Miri Tane") || !strings.Contains(body, "/u/miri") {
		t.Fatalf("with the default (on), the team page should list its members; body: %.800s", body)
	}

	setShowHostNames(t, h, apiKey, false)
	body := team()
	if strings.Contains(body, "Miri Tane") || strings.Contains(body, "/u/miri") {
		t.Errorf("member name/handle leaked into the team page with show_host_names off:\n%s", excerptAround(body, "Miri"))
	}
	if !strings.Contains(body, "Design") || !strings.Contains(body, "/book/"+slug) {
		t.Error("team name / event type links missing with show_host_names off")
	}

	// The person page keeps its own name.
	if code, pbody := personPage(t, h, "miri"); code != http.StatusOK || !strings.Contains(pbody, "Miri Tane") {
		t.Errorf("person page with show_host_names off: status %d, named = %t; want 200 and the person's name",
			code, strings.Contains(pbody, "Miri Tane"))
	}
}

// TestPublicSurfaces_noPoweredByAttribution: the "Powered by Calnode" footer is gone from
// every public booking surface (Apache-2.0; no attribution clause). The legal footer
// (privacy/terms/cookie settings/language) stays.
func TestPublicSurfaces_noPoweredByAttribution(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	patchBranding(t, h, apiKey, `{"privacy_url":"https://example.com/privacy"}`)

	bodies := map[string]string{"book": renderBookPage(t, h, slug)}

	start := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour).Add(9 * time.Hour)
	tok := issueTestToken(t, database, createBookingViaHTTP(t, h, slug, start.Format(time.RFC3339)))
	mreq := httptest.NewRequest(http.MethodGet, "/manage/"+tok, nil)
	mreq.SetPathValue("token", tok)
	mrec := httptest.NewRecorder()
	h.ManagePage(mrec, mreq)
	bodies["manage"] = mrec.Body.String()

	if code, _ := patchMeHandle(t, h, apiKey, `{"handle":"host"}`); code != http.StatusOK {
		t.Fatalf("set handle: %d", code)
	}
	_, bodies["person"] = personPage(t, h, "host")

	erec := httptest.NewRecorder()
	h.EmbedJS(erec, httptest.NewRequest(http.MethodGet, "/embed.js", nil))
	bodies["embed.js"] = erec.Body.String()

	for name, body := range bodies {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "powered by") || strings.Contains(lower, "powered_by") || strings.Contains(lower, "calnode.com") {
			t.Errorf("%s still carries the attribution footer:\n%s", name, excerptAround(body, "owered"))
		}
	}
	for _, name := range []string{"book", "manage", "person"} {
		if !strings.Contains(bodies[name], `class="legal-footer"`) || !strings.Contains(bodies[name], "https://example.com/privacy") {
			t.Errorf("%s lost the legal footer (privacy link) along with the attribution", name)
		}
	}
	if strings.Contains(bodies["embed.js"], ".powered{") || strings.Contains(bodies["embed.js"], "class: 'powered'") {
		t.Error("embed.js still carries the .powered footer element or CSS")
	}
}

// excerptAround returns a short window of body around the first occurrence of needle,
// so a leak assertion can show where the name surfaced instead of the whole page.
func excerptAround(body, needle string) string {
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	from, to := i-120, i+len(needle)+120
	if from < 0 {
		from = 0
	}
	if to > len(body) {
		to = len(body)
	}
	return "…" + body[from:to] + "…"
}
