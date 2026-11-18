package handler_test

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

func TestLivePage_localized(t *testing.T) {
	h, _, _, _ := setupWorkspaceWithDB(t)
	get := func(target, acceptLanguage string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if acceptLanguage != "" {
			req.Header.Set("Accept-Language", acceptLanguage)
		}
		rec := httptest.NewRecorder()
		h.LivePage(rec, req)
		mustStatus(t, rec, http.StatusOK, "live page "+target)
		if rec.Header().Get("Content-Security-Policy") == "" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors *") {
			t.Errorf("CSP changed: %q", rec.Header().Get("Content-Security-Policy"))
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("the framed live page must not persist ?lang= to a cookie: %v", rec.Result().Cookies())
		}
		return html.UnescapeString(rec.Body.String())
	}

	for _, c := range []struct{ name, target, al, code string }{
		{"English by default", "/live", "", "en"},
		{"Accept-Language fr", "/live", "fr-FR,fr;q=0.9", "fr"},
		{"?lang=de beats Accept-Language", "/live?lang=de", "fr", "de"},
		{"Canadian French", "/live", "fr-CA", "fr-CA"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := get(c.target, c.al)
			loc := i18n.Get(c.code)
			if !strings.Contains(body, `<html lang="`+c.code+`"`) {
				t.Errorf("want <html lang=%q>", c.code)
			}
			// Static text rendered through .T ...
			for _, k := range []string{"live_checking", "live_offline", "live_also_coming_up", "live_stale"} {
				if !strings.Contains(body, loc.T(k)) {
					t.Errorf("page lacks %s %q", k, loc.T(k))
				}
			}
			// ... and the finished strings its script builds the live/next markup from,
			// plus the locale its Intl calls use.
			if !strings.Contains(body, `"live_join_now":"`+loc.T("live_join_now")+`"`) {
				t.Errorf("script labels lack live_join_now %q", loc.T("live_join_now"))
			}
			if !strings.Contains(body, `var LOCALE = "`+c.code+`"`) {
				t.Errorf("script LOCALE is not %q", c.code)
			}
		})
	}
	if fr := get("/live", "fr"); strings.Contains(fr, "Also coming up") || !strings.Contains(fr, "Hors ligne") {
		t.Error("French page still carries the English heading, or lacks the French status word")
	}
}

func TestLiveStatus_labelsOnlyWhenLangGiven(t *testing.T) {
	h, _, _, _ := setupWorkspaceWithDB(t)

	plain := liveStatusBody(t, h, "")
	if _, ok := plain["labels"]; ok {
		t.Error("labels must not appear without ?lang= (existing consumers get the payload unchanged)")
	}
	if _, ok := plain["locale"]; ok {
		t.Error("locale must not appear without ?lang=")
	}
	for _, k := range []string{"live", "next", "upcoming"} {
		if _, ok := plain[k]; !ok {
			t.Errorf("existing field %q missing", k)
		}
	}

	for _, c := range []struct{ lang, want string }{
		{"es", "es"},
		{"es-MX", "es"},    // a host page's regional tag
		{"pt-BR", "pt"},    // ... matched like Accept-Language
		{"fr-ca", "fr-CA"}, // any case
		{"ja", "en"},       // unsupported: the operator's fallback (English by default)
		{"%21%21not%20a%20tag", "en"},
	} {
		body := liveStatusBody(t, h, "?lang="+c.lang) // c.lang is already URL-safe
		if body["locale"] != c.want {
			t.Errorf("lang=%q: locale = %v; want %q", c.lang, body["locale"], c.want)
		}
		labels, ok := body["labels"].(map[string]any)
		if !ok {
			t.Fatalf("lang=%q: labels = %v", c.lang, body["labels"])
		}
		loc := i18n.Get(c.want)
		for _, k := range []string{"live_now", "live_offline", "live_join_now", "live_hosted_by", "live_today_at", "live_next_session_at"} {
			if labels[k] != loc.T(k) {
				t.Errorf("lang=%q: labels[%s] = %v; want %q", c.lang, k, labels[k], loc.T(k))
			}
		}
		if _, ok := body["live"]; !ok {
			t.Errorf("lang=%q: existing fields must stay", c.lang)
		}
	}
	if es := liveStatusBody(t, h, "?lang=es")["labels"].(map[string]any); es["live_join_now"] == i18n.Default().T("live_join_now") {
		t.Error("Spanish labels are English")
	}

	// kind and lang combine.
	if body := liveStatusBody(t, h, "?kind=office_hours&lang=de"); body["locale"] != "de" {
		t.Errorf("kind+lang: locale = %v", body["locale"])
	}
}

func TestTeamCalendarEmbed_localized(t *testing.T) {
	h, _, adminKey, _ := setupWorkspaceWithDB(t)
	_, token := createShare(t, h, adminKey, "Dashboard")

	get := func(target, acceptLanguage string) (*httptest.ResponseRecorder, string) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if acceptLanguage != "" {
			req.Header.Set("Accept-Language", acceptLanguage)
		}
		rec := httptest.NewRecorder()
		h.TeamCalendarEmbed(rec, req)
		return rec, html.UnescapeString(rec.Body.String())
	}

	rec, body := get("/embed/team-calendar?token="+token, "fr")
	mustStatus(t, rec, http.StatusOK, "embed fr")
	fr := i18n.Get("fr")
	if !strings.Contains(body, `<html lang="fr">`) {
		t.Error(`want <html lang="fr">`)
	}
	for _, k := range []string{"team_calendar_title", "team_calendar_week", "team_calendar_month", "team_calendar_today", "team_calendar_previous", "close"} {
		if !strings.Contains(body, fr.T(k)) {
			t.Errorf("page lacks %s %q", k, fr.T(k))
		}
	}
	if !strings.Contains(body, `"team_calendar_busy":"`+fr.T("team_calendar_busy")+`"`) || !strings.Contains(body, `var LOCALE = "fr"`) {
		t.Error("script lacks the French labels or locale")
	}
	if strings.Contains(body, ">Week<") || strings.Contains(body, "Team calendar") {
		t.Error("French page still shows English chrome")
	}
	if !strings.Contains(body, token) {
		t.Error("the page must still carry the token")
	}

	// The 404 page is localized too, and ?lang= wins.
	rec, body = get("/embed/team-calendar?lang=de", "fr")
	mustStatus(t, rec, http.StatusNotFound, "embed 404 de")
	if de := i18n.Get("de"); !strings.Contains(body, de.T("team_calendar_not_found")) || !strings.Contains(body, `<html lang="de">`) {
		t.Errorf("404 page not German: %s", body)
	}

	// English stays the default.
	_, body = get("/embed/team-calendar?token="+token, "")
	if !strings.Contains(body, ">Week<") || !strings.Contains(body, `<html lang="en">`) {
		t.Error("default page should be English")
	}
}
