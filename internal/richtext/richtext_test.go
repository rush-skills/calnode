package richtext

import "testing"

func TestSanitize_keepsAllowedFormattingAndDropsTheRest(t *testing.T) {
	in := `<h2>Agenda</h2><p>Bring <strong>questions</strong> and <em>ideas</em>.<br>See <a href="https://example.com/prep">prep notes</a>.</p>` +
		`<ul><li>One</li><li>Two</li></ul><blockquote>quoted</blockquote>` +
		`<script>alert(1)</script><img src="x" onerror="alert(1)"><div style="color:red">styled</div>` +
		`<a href="javascript:alert(1)">bad link</a><p onclick="x()">click</p>`
	got := Sanitize(in)
	for _, want := range []string{
		"<h2>Agenda</h2>", "<strong>questions</strong>", "<em>ideas</em>", "<br>",
		`<a href="https://example.com/prep" rel="nofollow noopener" target="_blank">prep notes</a>`,
		"<ul><li>One</li><li>Two</li></ul>", "<blockquote>quoted</blockquote>",
		"styled", "bad link", "<p>click</p>",
	} {
		if !contains(got, want) {
			t.Errorf("sanitized output missing %q\n---\n%s", want, got)
		}
	}
	for _, banned := range []string{"<script", "<img", "onerror", "style=", "javascript:", "onclick", "<div"} {
		if contains(got, banned) {
			t.Errorf("sanitized output still contains %q\n---\n%s", banned, got)
		}
	}
}

func TestSanitize_escapesStrayAngleBrackets(t *testing.T) {
	got := Sanitize("<p>a < b and c > d</p>")
	if got != "<p>a &lt; b and c &gt; d</p>" {
		t.Errorf("got %q", got)
	}
}

func TestSanitize_allowsMailtoAndRejectsOtherSchemes(t *testing.T) {
	got := Sanitize(`<a href="mailto:hi@example.com">mail</a> <a href="ftp://x">ftp</a>`)
	if !contains(got, `href="mailto:hi@example.com"`) {
		t.Errorf("mailto link should survive: %q", got)
	}
	if contains(got, "ftp://") {
		t.Errorf("ftp link should be stripped: %q", got)
	}
}

func TestSanitize_emptyAndStructuralOnlyBecomeEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "<p></p>", "<ul></ul><p> </p>", "<script>x</script>", "<div></div>"} {
		if got := Sanitize(in); got != "" {
			t.Errorf("Sanitize(%q) = %q; want empty", in, got)
		}
	}
	// A lone line break is content the author placed on purpose; it is kept.
	if got := Sanitize("<p><br></p>"); got != "<p><br></p>" {
		t.Errorf("Sanitize(<p><br></p>) = %q", got)
	}
}

func TestToPlainText_rendersBlocksListsLinksAndEntities(t *testing.T) {
	in := `<h1>Welcome</h1><p>First &amp; foremost.<br>Second line.</p>` +
		`<ul><li>Alpha</li><li>Beta</li></ul>` +
		`<p>See <a href="https://example.com/x">the guide</a> or <a href="https://example.com/y">https://example.com/y</a>.</p>` +
		`<p>Ping <a href="mailto:a@b.c"><b>us</b></a></p>`
	want := "Welcome\n\nFirst & foremost.\nSecond line.\n\n- Alpha\n- Beta\n\nSee the guide (https://example.com/x) or https://example.com/y.\n\nPing us (mailto:a@b.c)"
	if got := ToPlainText(in); got != want {
		t.Errorf("ToPlainText =\n%q\nwant\n%q", got, want)
	}
}

func TestToPlainText_plainInputPassesThrough(t *testing.T) {
	if got := ToPlainText("just text\r\nwith lines  \n"); got != "just text\nwith lines" {
		t.Errorf("got %q", got)
	}
	if got := ToPlainText(""); got != "" {
		t.Errorf("empty in, got %q", got)
	}
}

func TestToPlainText_emptyAnchorTextFallsBackToHref(t *testing.T) {
	if got := ToPlainText(`<a href="https://e.x/"></a>`); got != "https://e.x/" {
		t.Errorf("got %q", got)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
