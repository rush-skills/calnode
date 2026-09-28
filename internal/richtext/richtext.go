// Package richtext is the one place admin-authored HTML is made safe and turned into
// plain text. Rich content (a calendar message, an email note) is written in the admin
// editor, stored as HTML, and then reaches surfaces with very different tolerances: a
// Google or Outlook invite renders HTML, an .ics DESCRIPTION and a text/plain email part
// take only text, and a calendar deep link goes through a query string. Sanitize runs on
// the way in (save) and again on the way out (send), so a row written by an older
// version or straight into the table is still held to the same allowlist.
package richtext

import (
	"html"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// policy is the allowlist: inline formatting, paragraphs, lists, three heading levels,
// block quotes and links. Nothing that carries script, style, media or layout. Links
// are limited to http(s) and mailto, opened in a new tab with rel=nofollow so an
// admin's link never inherits the recipient's session or passes referrer weight.
var policy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "strong", "b", "em", "i", "u", "s", "ul", "ol", "li",
		"h1", "h2", "h3", "blockquote")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireParseableURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// Sanitize reduces html to the allowlist above. Whitespace-only input, or input that
// is nothing but disallowed markup, comes back as "" so callers can treat "no message"
// uniformly. Text is HTML-escaped by bluemonday, so a stray "<" in prose survives as
// "&lt;", never as a tag.
func Sanitize(input string) string {
	out := strings.TrimSpace(policy.Sanitize(input))
	if strings.TrimSpace(ToPlainText(out)) == "" && !strings.Contains(out, "<br") {
		// Only structural leftovers such as "<p></p>": there is nothing to show.
		return ""
	}
	return out
}

var (
	reBlockClose = regexp.MustCompile(`(?i)</(p|div|h[1-6]|blockquote|ul|ol|tr)>`)
	reBlockOpen  = regexp.MustCompile(`(?i)<(p|div|h[1-6]|blockquote|ul|ol|tr)(\s[^>]*)?>`)
	reBr         = regexp.MustCompile(`(?i)<br\s*/?>`)
	reLi         = regexp.MustCompile(`(?i)<li(\s[^>]*)?>`)
	reAnchor     = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	reTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlankRuns  = regexp.MustCompile(`\n{3,}`)
	reLineSpace  = regexp.MustCompile(`[ \t]+\n`)
)

// ToPlainText renders HTML as readable text: paragraphs and headings become
// blank-line-separated blocks, list items get a leading "- ", <br> becomes a newline,
// and a link whose text differs from its target keeps the target in parentheses so it
// is not lost in a text-only invite. Entities are decoded. Input is expected to be
// sanitized, but nothing here relies on that: every tag is stripped regardless.
func ToPlainText(input string) string {
	s := input
	s = reAnchor.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAnchor.FindStringSubmatch(m)
		href, text := sub[1], strings.TrimSpace(reTag.ReplaceAllString(sub[2], ""))
		if text == "" || text == href {
			return href
		}
		return text + " (" + href + ")"
	})
	s = reBr.ReplaceAllString(s, "\n")
	s = reLi.ReplaceAllString(s, "\n- ")
	s = reBlockOpen.ReplaceAllString(s, "\n")
	s = reBlockClose.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reLineSpace.ReplaceAllString(s, "\n")
	// Collapse the doubled newlines a closing tag followed by an opening tag produce
	// into at most one blank line, and trim the ends.
	s = reBlankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
