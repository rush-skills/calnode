package handler

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/i18n"
)

//go:embed templates/live.html
var liveTmplSrc string

var liveTmpl = template.Must(template.New("live").Parse(liveTmplSrc))

//go:embed live-widget.js
var liveWidgetJS []byte

// liveWidgetJSVersion is a content-hash ETag for /live-widget.js, for the same reason
// embed.js has one: host pages link it unversioned, so a redeploy must revalidate cheaply.
var liveWidgetJSVersion = func() string {
	sum := sha256.Sum256(liveWidgetJS)
	return `"` + hex.EncodeToString(sum[:])[:16] + `"`
}()

// livePageCSP: inline script + styles (the page is self-contained), status polling to this
// origin only, and - the point of the page - frameable from anywhere. The booking pages
// deny framing; this one exists to be put in an <iframe> on the operator's own site.
const livePageCSP = "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; frame-ancestors *"

// liveLabelKeys are the catalogue strings the /live page's script and the <calnode-live>
// widget build their markup from. Both receive them as one finished-string object
// (liveLabels): the page inline, the widget as the status feed's `labels` when it asks
// with ?lang=. Nothing in either is pluralised or reordered client-side; the only
// substitution is %s / %[n]s for values the feed returns after load (a host name, a
// session title, an Intl-formatted time in the viewer's zone), the same exception
// CLAUDE.md makes for the booking surfaces' selected date.
//
// TestLiveSurfaces_i18nKeysExist checks that every t('…') lookup in live.html and
// live-widget.js is listed here and defined in en.json.
var liveLabelKeys = []string{
	"live_page_title",
	"live_checking",
	"live_error",
	"live_stale",
	"live_unavailable",
	"live_now",
	"live_offline",
	"live_join_now",
	"live_hosted_by",
	"live_next_session",
	"live_next_session_at",
	"live_no_session",
	"live_also_coming_up",
	"live_today_at",
	"live_tomorrow_at",
	"live_date_at",
	"live_kind_office_hours",
	"live_kind_event",
}

// localeLabels returns loc's finished strings for keys, for a page script or a widget
// that cannot call {{.T}}. Missing translations fall back to English per key (Locale.T).
func localeLabels(loc *i18n.Locale, keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = loc.T(k)
	}
	return out
}

// liveLabels is localeLabels for the live-status surfaces.
func liveLabels(loc *i18n.Locale) map[string]string { return localeLabels(loc, liveLabelKeys) }

// liveStatusLangMax bounds the ?lang= value the status feed will try to match: a BCP-47
// tag is short, and the value comes from an arbitrary third-party page.
const liveStatusLangMax = 35

// liveStatusLocale resolves the status feed's ?lang= (sent by the widget from data-lang,
// the host page's <html lang>, or navigator.language). An exact locale code wins
// ("fr-CA"); otherwise it is matched like a one-entry Accept-Language, so "pt-BR" or
// "de-AT" find pt / de. No match falls back to the operator's fallback locale, exactly as
// the booking page does for an unsupported browser language. The visitor's own
// Accept-Language is deliberately not consulted: the widget renders inside someone
// else's page, and that page's language is the one the label should match.
func (h *Handler) liveStatusLocale(r *http.Request, lang string) *i18n.Locale {
	if len(lang) > liveStatusLangMax {
		lang = lang[:liveStatusLangMax]
	}
	return i18n.ResolveWithFallback(lang, lang, h.loadBranding(r.Context()).FallbackLocale)
}

// addLiveStatusLabels adds `locale` and `labels` to a status-feed payload when the
// request names a language. Additive only: without ?lang= the payload is unchanged, so
// existing consumers of the feed see exactly the fields they always did.
func (h *Handler) addLiveStatusLabels(r *http.Request, resp map[string]any) {
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		return
	}
	loc := h.liveStatusLocale(r, lang)
	resp["locale"] = loc.Code
	resp["labels"] = liveLabels(loc)
}

type livePageData struct {
	Kind         string
	Theme        string
	BusinessName string
	LogoURL      string
	DemoMode     bool
	Locale       string
	T            func(string) string
	// Labels is JSON-encoded by html/template in the script context (as Kind is).
	Labels map[string]string
}

// LivePage handles GET /live?kind=&theme=light|dark[&lang=]: the embeddable live-status
// page. No-store because its whole content is "is anyone live right now". The language is
// resolved like the booking page's (?lang=, the calnode_lang cookie, Accept-Language, the
// operator's fallback); ?lang= is not persisted to the cookie here, because on a framed
// page it is the embedding site's configuration, not the visitor's choice.
func (h *Handler) LivePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "" && !validLiveKind(kind) {
		kind = ""
	}
	theme := "light"
	if q.Get("theme") == "dark" {
		theme = "dark"
	}
	brand := h.loadBranding(r.Context())
	loc := h.resolveLocaleWithFallback(r, brand.FallbackLocale)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", livePageCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Accept-Language, Cookie")
	if err := liveTmpl.Execute(w, livePageData{
		Kind: kind, Theme: theme, BusinessName: brand.BusinessName, LogoURL: brand.LogoURL, DemoMode: h.demoMode,
		Locale: loc.Code, T: loc.T, Labels: liveLabels(loc),
	}); err != nil {
		h.logger.ErrorContext(r.Context(), "live page: template", "error", err)
	}
}

// LiveWidgetJS serves the <calnode-live> web component at GET /live-widget.js with the
// same caching contract as /embed.js (short max-age + content-hash ETag, CORS-public).
// The file is the same bytes for every language: the widget gets its strings from the
// status feed (?lang= → `labels`), so the ETag stays a plain content hash.
func (h *Handler) LiveWidgetJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("ETag", liveWidgetJSVersion)
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "live-widget.js", time.Time{}, bytes.NewReader(liveWidgetJS))
}
