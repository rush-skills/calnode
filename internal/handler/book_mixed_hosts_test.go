package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A round-robin event whose rotation is joined by always-attending hosts (the
// "Some always attend" staffing mode) shows the required hosts alongside the
// rotation pool on the booking page — those people are in every meeting, so hiding
// them made the header lie. Optional always-attend hosts stay hidden: they join
// only when free, so a face would be a promise the booking may not keep.
func TestBookPage_roundRobinWithFixedHosts_showsRequiredNotOptional(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)
	for _, u := range [][2]string{{"rot1", "Rota One"}, {"rot2", "Rota Two"}, {"req1", "Lead Always"}, {"opt1", "Maybe Joins"}} {
		if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES (?,?,?,'UTC',0)`, u[0], u[0]+"@example.com", u[1]); err != nil {
			t.Fatalf("seed user %s: %v", u[0], err)
		}
	}
	slug, _ := seedEventTypeHTTP(t, h, key)
	makeRoundRobin(t, h, slug, key, "rot1", "rot2")
	// Re-put the host list with the two fixed roles the editor's mixed mode writes.
	rec := putHosts(t, h, slug, key, `{"hosts":[
		{"user_id":"rot1","role":"rotation","priority":0},
		{"user_id":"rot2","role":"rotation","priority":1},
		{"user_id":"req1","role":"required","priority":0},
		{"user_id":"opt1","role":"optional","priority":1}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set mixed hosts: %d — %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/book/"+slug, nil)
	req.SetPathValue("slug", slug)
	rec = httptest.NewRecorder()
	h.BookPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The faces stack renders initials only; the label under it lists first names in
	// the same order, so that is what pins down who is shown and in what order.
	i := strings.Index(body, `id="host-name"`)
	if i < 0 {
		t.Fatalf("no host-name label in page:\n%s", body)
	}
	label := body[i : i+strings.Index(body[i:], "</p>")]
	if want := "Lead, Rota &amp; Rota"; !strings.Contains(label, want) {
		t.Errorf("host label = %q; want it to read %q (required first, then the rotation pool, no optional host)", label, want)
	}
	if strings.Contains(label, "Maybe") {
		t.Errorf("optional host must not be promised on the booking page; got %q", label)
	}
}
