package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// --- settings endpoint ---------------------------------------------------------

func getSignin(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	GetSigninSettings(http.ResponseWriter, *http.Request)
}, key string) (int, map[string]any) {
	t.Helper()
	req := authReq(http.MethodGet, "/v1/settings/signin", "", key)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetSigninSettings)(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func patchSignin(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	PatchSigninSettings(http.ResponseWriter, *http.Request)
}, key, body string) (int, map[string]any) {
	t.Helper()
	req := authReq(http.MethodPatch, "/v1/settings/signin", body, key)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchSigninSettings)(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestSigninSettings_roundTripValidationAndAccess(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)

	if code, out := getSignin(t, h, adminKey); code != http.StatusOK || fmt.Sprint(out["allowed_signin_domains"]) != "[]" {
		t.Errorf("initial GET: %d %v; want 200 and an empty list", code, out)
	}
	code, out := patchSignin(t, h, adminKey, `{"allowed_signin_domains":["@Acme.com","acme.com, example.org"]}`)
	if code != http.StatusOK || fmt.Sprint(out["allowed_signin_domains"]) != "[acme.com example.org]" {
		t.Errorf("PATCH: %d %v", code, out)
	}
	var stored string
	if err := database.QueryRow(`SELECT allowed_signin_domains FROM server_settings WHERE id = 1`).Scan(&stored); err != nil || stored != "acme.com,example.org" {
		t.Errorf("stored = %q (err %v)", stored, err)
	}
	// Omitting the field keeps it; an invalid entry is named; an empty list clears.
	if code, out := patchSignin(t, h, adminKey, `{}`); code != http.StatusOK || fmt.Sprint(out["allowed_signin_domains"]) != "[acme.com example.org]" {
		t.Errorf("PATCH {}: %d %v", code, out)
	}
	if code, out := patchSignin(t, h, adminKey, `{"allowed_signin_domains":["bad_domain"]}`); code != http.StatusBadRequest || fmt.Sprint(out["error"]) != `"bad_domain" is not a valid domain` {
		t.Errorf("PATCH invalid: %d %v", code, out)
	}
	if code, _ := patchSignin(t, h, adminKey, `{bad json`); code != http.StatusBadRequest {
		t.Errorf("PATCH bad json: %d", code)
	}
	if code, out := patchSignin(t, h, adminKey, `{"allowed_signin_domains":[]}`); code != http.StatusOK || fmt.Sprint(out["allowed_signin_domains"]) != "[]" {
		t.Errorf("PATCH clear: %d %v", code, out)
	}

	// Non-admin members are refused on both verbs.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','M','UTC',0)`) //nolint:errcheck
	const memberKey = "cno_memberkey"
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(memberKey)) //nolint:errcheck
	if code, _ := getSignin(t, h, memberKey); code != http.StatusForbidden {
		t.Errorf("member GET: %d; want 403", code)
	}
	if code, _ := patchSignin(t, h, memberKey, `{"allowed_signin_domains":["acme.com"]}`); code != http.StatusForbidden {
		t.Errorf("member PATCH: %d; want 403", code)
	}
}

// The settings row is seeded by migration, but a wiped one must read as invite-only,
// not as an error.
func TestSigninSettings_missingRowReadsAsEmpty(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	if _, err := database.Exec(`DELETE FROM server_settings`); err != nil {
		t.Fatal(err)
	}
	if code, out := getSignin(t, h, adminKey); code != http.StatusOK || fmt.Sprint(out["allowed_signin_domains"]) != "[]" {
		t.Errorf("GET without row: %d %v; want 200 and []", code, out)
	}
}

func TestSigninSettings_demoModeRefusesWrites(t *testing.T) {
	h, _, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetDemoMode(true)
	if code, _ := patchSignin(t, h, adminKey, `{"allowed_signin_domains":["acme.com"]}`); code != http.StatusServiceUnavailable {
		t.Errorf("demo PATCH: %d; want 503", code)
	}
	if code, _ := getSignin(t, h, adminKey); code != http.StatusOK {
		t.Errorf("demo GET: %d; want 200", code)
	}
}

func TestSigninSettings_queryErrorIs500(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	if _, err := database.Exec(`DROP TABLE server_settings`); err != nil {
		t.Fatal(err)
	}
	if code, _ := getSignin(t, h, adminKey); code != http.StatusInternalServerError {
		t.Errorf("GET without table: %d; want 500", code)
	}
	if code, _ := patchSignin(t, h, adminKey, `{"allowed_signin_domains":["acme.com"]}`); code != http.StatusInternalServerError {
		t.Errorf("PATCH without table: %d; want 500", code)
	}
}

// --- OAuth callbacks -------------------------------------------------------------

// providerStub answers the token exchange and the user-info call for both providers
// in-process: golang.org/x/oauth2 takes its HTTP client from the request context, so
// the callback under test talks to this instead of the internet.
type providerStub struct {
	userinfo string // JSON for Google's userinfo or Graph /me
	status   int
	infoErr  error // returned for the user-info call, to stand in for a network failure
}

func (p providerStub) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.infoErr != nil && !strings.Contains(r.URL.Path, "/token") {
		return nil, p.infoErr
	}
	body, status := "", http.StatusOK
	switch {
	case strings.Contains(r.URL.Path, "/token"):
		body = `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`
	default:
		body, status = p.userinfo, p.status
		if status == 0 {
			status = http.StatusOK
		}
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func callbackWith(t *testing.T, handle http.HandlerFunc, path string, stub providerStub) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"?state=abc&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: "calnode_oauth_state", Value: "abc"})
	req = req.WithContext(context.WithValue(req.Context(), oauth2.HTTPClient, &http.Client{Transport: stub}))
	rec := httptest.NewRecorder()
	handle(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "calnode_session" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func setDomains(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	PatchSigninSettings(http.ResponseWriter, *http.Request)
}, key string, domains ...string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"allowed_signin_domains": domains})
	if code, out := patchSignin(t, h, key, string(b)); code != http.StatusOK {
		t.Fatalf("set domains: %d %v", code, out)
	}
}

func TestCallbackGoogle_allowedDomainProvisionsMember(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)
	setDomains(t, h, adminKey, "acme.com")

	rec := callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"new@acme.com","name":"New Person","verified_email":true}`})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin" {
		t.Fatalf("status=%d location=%q; want 302 to /admin — body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if sessionCookie(rec) == "" {
		t.Error("no session cookie set")
	}
	var name string
	var isAdmin, emailLogin int
	if err := database.QueryRow(`SELECT name, is_admin, email_login FROM users WHERE email = 'new@acme.com'`).Scan(&name, &isAdmin, &emailLogin); err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if name != "New Person" || isAdmin != 0 || emailLogin != 0 {
		t.Errorf("user = name %q admin %d email_login %d; want New Person, 0, 0", name, isAdmin, emailLogin)
	}

	// Second sign-in finds the account rather than creating another.
	rec = callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"new@acme.com","name":"Renamed","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin" {
		t.Errorf("second sign-in: %q", rec.Header().Get("Location"))
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'new@acme.com'`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("users with that email = %d; want 1", n)
	}
}

func TestCallbackGoogle_refusesUnverifiedOtherDomainAndEmptyList(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)

	// No domains configured: invite-only, as before.
	rec := callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"x@acme.com","name":"X","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=no_account" {
		t.Errorf("empty list: %q", rec.Header().Get("Location"))
	}
	setDomains(t, h, adminKey, "acme.com")
	// Unverified address on an allowed domain: refused before the lookup even runs
	// (fetchGoogleUserInfo rejects it), so it surfaces as the user-info error. Google
	// lets an account claim any address; only a verified one may mint a member.
	rec = callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"x@acme.com","name":"X","verified_email":false}`})
	if rec.Header().Get("Location") != "/admin/login?error=userinfo" {
		t.Errorf("unverified: %q", rec.Header().Get("Location"))
	}
	// Verified, but a different domain.
	rec = callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"x@other.com","name":"X","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=no_account" {
		t.Errorf("other domain: %q", rec.Header().Get("Location"))
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE email IN ('x@acme.com','x@other.com')`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("refused sign-ins created %d users", n)
	}
	// An archived member on an allowed domain stays archived, not re-provisioned.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,archived_at) VALUES ('gone','gone@acme.com','Gone','UTC','2024-01-01T00:00:00Z')`) //nolint:errcheck
	rec = callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"gone@acme.com","name":"Gone","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=archived" {
		t.Errorf("archived: %q", rec.Header().Get("Location"))
	}
}

func TestCallbackGoogle_nameFallsBackToLocalPart(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)
	setDomains(t, h, adminKey, "acme.com")
	callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"pat.lee@acme.com","name":"  ","verified_email":true}`})
	var name string
	if err := database.QueryRow(`SELECT name FROM users WHERE email = 'pat.lee@acme.com'`).Scan(&name); err != nil || name != "pat.lee" {
		t.Errorf("name = %q (err %v); want pat.lee", name, err)
	}
}

func TestCallbackGoogle_provisionFailureRedirectsToSessionError(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)
	setDomains(t, h, adminKey, "acme.com")
	// Make the insert fail: a trigger that aborts every users insert.
	if _, err := database.Exec(`CREATE TRIGGER no_insert BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'nope'); END`); err != nil {
		t.Fatal(err)
	}
	rec := callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"new@acme.com","name":"New","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=session" {
		t.Errorf("location = %q; want error=session", rec.Header().Get("Location"))
	}
}

func TestCallbackGoogle_lookupFailureRedirectsToSessionError(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)
	if _, err := database.Exec(`DROP TABLE users`); err != nil {
		t.Fatal(err)
	}
	rec := callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"new@acme.com","name":"New","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=session" {
		t.Errorf("location = %q; want error=session", rec.Header().Get("Location"))
	}
}

func TestCallbackGoogle_domainLoadFailureStillRefusesUnknown(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	h.SetGoogleAuth("id", "secret", "http://localhost/v1/auth/callback", false)
	if _, err := database.Exec(`DROP TABLE server_settings`); err != nil {
		t.Fatal(err)
	}
	rec := callbackWith(t, h.CallbackGoogle, "/v1/auth/callback",
		providerStub{userinfo: `{"email":"new@acme.com","name":"New","verified_email":true}`})
	if rec.Header().Get("Location") != "/admin/login?error=no_account" {
		t.Errorf("location = %q; want no_account when the domain list cannot be read", rec.Header().Get("Location"))
	}
}

// Microsoft sign-in never auto-provisions: Graph's mail attribute is admin-settable in
// any tenant and not domain-verified (MSRC "nOAuth"), so an allowed domain is not proof.
func TestCallbackMicrosoft_allowedDomainDoesNotProvision(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetMicrosoftAuth("id", "secret", "common", "http://localhost/v1/auth/microsoft/callback", false)
	setDomains(t, h, adminKey, "acme.com")

	rec := callbackWith(t, h.CallbackMicrosoft, "/v1/auth/microsoft/callback",
		providerStub{userinfo: `{"mail":"Sam@Acme.com","userPrincipalName":"sam@acme.com","displayName":"Sam Rivera"}`})
	if rec.Header().Get("Location") != "/admin/login?error=no_account" {
		t.Fatalf("status=%d location=%q; want no_account", rec.Code, rec.Header().Get("Location"))
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'sam@acme.com'`).Scan(&n)
	if n != 0 {
		t.Errorf("user rows = %d; want 0", n)
	}

	// Not on the list: refused, nothing created.
	rec = callbackWith(t, h.CallbackMicrosoft, "/v1/auth/microsoft/callback",
		providerStub{userinfo: `{"mail":"o@other.com","userPrincipalName":"o@other.com","displayName":"O"}`})
	if rec.Header().Get("Location") != "/admin/login?error=no_account" {
		t.Errorf("other domain: %q", rec.Header().Get("Location"))
	}
	// A guest identity never resolves to an email at all.
	rec = callbackWith(t, h.CallbackMicrosoft, "/v1/auth/microsoft/callback",
		providerStub{userinfo: `{"mail":"","userPrincipalName":"g_acme.com#EXT#@tenant.onmicrosoft.com","displayName":"G"}`})
	if rec.Header().Get("Location") != "/admin/login?error=userinfo" {
		t.Errorf("guest: %q", rec.Header().Get("Location"))
	}
}

// Every way Graph /me can fail to yield an address ends at the same userinfo error,
// without touching the users table.
func TestCallbackMicrosoft_userInfoFailures(t *testing.T) {
	h, database, adminKey, _ := setupWorkspaceWithDB(t)
	h.SetMicrosoftAuth("id", "secret", "common", "http://localhost/v1/auth/microsoft/callback", false)
	setDomains(t, h, adminKey, "acme.com")
	for name, stub := range map[string]providerStub{
		"network":   {infoErr: errors.New("boom")},
		"status":    {userinfo: `{}`, status: http.StatusInternalServerError},
		"badjson":   {userinfo: `{not json`},
		"noaddress": {userinfo: `{"mail":"","userPrincipalName":"","displayName":"X"}`},
	} {
		rec := callbackWith(t, h.CallbackMicrosoft, "/v1/auth/microsoft/callback", stub)
		if rec.Header().Get("Location") != "/admin/login?error=userinfo" {
			t.Errorf("%s: location = %q; want error=userinfo", name, rec.Header().Get("Location"))
		}
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("users = %d; want only the workspace owner", n)
	}
}
