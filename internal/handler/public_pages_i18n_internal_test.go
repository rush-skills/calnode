package handler

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/i18n"
)

// templateTKeys returns the keys a template renders through {{call .T "…"}}.
func templateTKeys(src string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`\{\{call \.T "([a-z_]+)"\}\}`).FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

func requireEnglishKey(t *testing.T, where, key string) {
	t.Helper()
	if i18n.Default().T(key) == key {
		t.Errorf("%s looks up %q, which en.json does not define - Locale.T would show the key itself to a visitor", where, key)
	}
}

// Every string the /live page and the <calnode-live> widget look up must be in the label
// set the server sends them (liveLabelKeys) and in en.json: their t() falls back to the
// key itself, so a miss renders "live_join_now" on a customer's site with nothing failing.
func TestLiveSurfaces_i18nKeysExist(t *testing.T) {
	for _, k := range liveLabelKeys {
		requireEnglishKey(t, "liveLabelKeys", k)
	}
	for _, k := range templateTKeys(liveTmplSrc) {
		requireEnglishKey(t, "live.html", k)
	}

	pageLookups := regexp.MustCompile(`\bt\('([a-z_]+)'\)`).FindAllStringSubmatch(liveTmplSrc, -1)
	widgetLookups := regexp.MustCompile(`\bt\((?:labels|that\._labels), '([a-z_]+)'\)`).FindAllStringSubmatch(string(liveWidgetJS), -1)
	if len(pageLookups) < 8 || len(widgetLookups) < 8 {
		t.Fatalf("found %d page / %d widget t() lookups; has the lookup syntax changed?", len(pageLookups), len(widgetLookups))
	}
	for name, ms := range map[string][][]string{"live.html": pageLookups, "live-widget.js": widgetLookups} {
		for _, m := range ms {
			if !slices.Contains(liveLabelKeys, m[1]) {
				t.Errorf("%s looks up %q, which liveLabelKeys does not send", name, m[1])
			}
		}
	}

	// The widget's built-in English (shown before the first answer, or if the server
	// never answers) must be the catalogue's English, and cover every key it looks up.
	en := map[string]string{}
	block := regexp.MustCompile(`(?s)var EN = \{(.*?)\n  \};`).FindStringSubmatch(string(liveWidgetJS))
	if block == nil {
		t.Fatal("live-widget.js has no EN fallback table")
	}
	for _, m := range regexp.MustCompile(`(\w+): '((?:[^'\\]|\\.)*)'`).FindAllStringSubmatch(block[1], -1) {
		v, err := strconv.Unquote(`"` + m[2] + `"`)
		if err != nil {
			t.Fatalf("EN.%s: %v", m[1], err)
		}
		en[m[1]] = v
		if want := i18n.Default().T(m[1]); v != want {
			t.Errorf("live-widget.js EN.%s = %q; en.json says %q", m[1], v, want)
		}
	}
	for _, m := range widgetLookups {
		if _, ok := en[m[1]]; !ok {
			t.Errorf("live-widget.js looks up %q but has no English fallback for it", m[1])
		}
	}
}

func TestTeamCalendarEmbed_i18nKeysExist(t *testing.T) {
	for _, k := range teamCalendarLabelKeys {
		requireEnglishKey(t, "teamCalendarLabelKeys", k)
	}
	for _, k := range templateTKeys(teamCalendarTmplSrc) {
		requireEnglishKey(t, "team-calendar.html", k)
	}
	lookups := regexp.MustCompile(`\bt\('([a-z_]+)'\)`).FindAllStringSubmatch(teamCalendarTmplSrc, -1)
	if len(lookups) < 10 {
		t.Fatalf("found only %d t() lookups in team-calendar.html; has the lookup syntax changed?", len(lookups))
	}
	for _, m := range lookups {
		if !slices.Contains(teamCalendarLabelKeys, m[1]) {
			t.Errorf("team-calendar.html looks up %q, which teamCalendarLabelKeys does not send", m[1])
		}
	}
}

// No English sentence may survive in the scripts' rendered output: the strings they used
// to hardcode are now catalogue lookups. A cheap tripwire for a reverted or new literal.
func TestPublicPages_noHardcodedEnglishLabels(t *testing.T) {
	for name, src := range map[string]string{
		"live.html":          liveTmplSrc,
		"live-widget.js":     string(liveWidgetJS),
		"team-calendar.html": teamCalendarTmplSrc,
	} {
		code := src
		if name == "live-widget.js" { // the EN fallback table is the one allowed copy
			code = regexp.MustCompile(`(?s)var EN = \{.*?\n  \};`).ReplaceAllString(code, "")
		}
		for _, s := range []string{"'Live now'", "'Join now'", "'Hosted by ", "'Next session", "'Offline'",
			"'No session scheduled", "'Today at ", "'All day'", "' more'", "'Calnode booking'", "['When'",
			"'This share link", "'Could not load"} {
			if strings.Contains(code, s) {
				t.Errorf("%s still hardcodes %s", name, s)
			}
		}
	}
}
