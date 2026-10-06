package handler

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"time"
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

type livePageData struct {
	Kind         string
	Theme        string
	BusinessName string
	LogoURL      string
	DemoMode     bool
}

// LivePage handles GET /live?kind=&theme=light|dark: the embeddable live-status page.
// No-store because its whole content is "is anyone live right now".
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", livePageCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := liveTmpl.Execute(w, livePageData{
		Kind: kind, Theme: theme, BusinessName: brand.BusinessName, LogoURL: brand.LogoURL, DemoMode: h.demoMode,
	}); err != nil {
		h.logger.ErrorContext(r.Context(), "live page: template", "error", err)
	}
}

// LiveWidgetJS serves the <calnode-live> web component at GET /live-widget.js with the
// same caching contract as /embed.js (short max-age + content-hash ETag, CORS-public).
func (h *Handler) LiveWidgetJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("ETag", liveWidgetJSVersion)
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "live-widget.js", time.Time{}, bytes.NewReader(liveWidgetJS))
}
