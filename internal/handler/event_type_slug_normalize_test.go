package handler_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// createET posts a raw body to POST /v1/event-types and returns the recorder.
func createET(t *testing.T, h *handler.Handler, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := authReq(http.MethodPost, "/v1/event-types", body, apiKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateEventType)(rec, req)
	return rec
}

// The create path stores the slug exactly as typed until now, so "Intro Call" made a
// /book/ link with a space in it that never resolved. Every create now normalises.
func TestCreateEventType_normalisesSlug(t *testing.T) {
	cases := []struct {
		name, slug, want string
	}{
		{"spaces and caps", "  Intro Call  ", "intro-call"},
		{"mixed case", "DemoCall", "democall"},
		{"underscores", "demo_call_2", "demo-call-2"},
		{"unicode is dropped, not rejected", "Café ✨ Call", "caf-call"},
		{"already canonical", "intro-call-v2", "intro-call-v2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, apiKey, _ := setupWorkspace(t)
			rec := createET(t, h, apiKey,
				fmt.Sprintf(`{"slug":%q,"name":"Intro Call","duration_minutes":30}`, tc.slug))
			body := mustCreated(t, rec, "create")
			if got := mustString(t, body, "slug", "create"); got != tc.want {
				t.Errorf("slug = %q, want %q", got, tc.want)
			}
			// And the booking page resolves under the slug the server returned.
			slug := mustString(t, body, "slug", "create")
			pageReq := httptest.NewRequest(http.MethodGet, "/book/"+slug, nil)
			pageReq.SetPathValue("slug", slug)
			pageRec := httptest.NewRecorder()
			h.BookPage(pageRec, pageReq)
			if pageRec.Code != http.StatusOK {
				t.Errorf("/book/%s: %d; want 200", slug, pageRec.Code)
			}
		})
	}
}

// An omitted slug derives from the name, as teams do.
func TestCreateEventType_derivesSlugFromName(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	rec := createET(t, h, apiKey, `{"name":"Quick Chat (15 min)","duration_minutes":15}`)
	body := mustCreated(t, rec, "create")
	if got := mustString(t, body, "slug", "create"); got != "quick-chat-15-min" {
		t.Errorf("slug = %q, want quick-chat-15-min", got)
	}
}

// A slug that normalises to nothing is a 400, not a silent fallback: the caller said
// what they wanted and it cannot be honoured.
func TestCreateEventType_rejectsSlugWithNoLettersOrNumbers(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	rec := createET(t, h, apiKey, `{"slug":"!!! ---","name":"Intro Call","duration_minutes":30}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with unusable slug: %d; want 400 — %s", rec.Code, rec.Body.String())
	}
	// A name that yields nothing either, with no slug, is the same 400.
	rec = createET(t, h, apiKey, `{"name":"???","duration_minutes":30}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with unusable name and no slug: %d; want 400 — %s", rec.Code, rec.Body.String())
	}
	// Name is still required on its own.
	rec = createET(t, h, apiKey, `{"slug":"ok","duration_minutes":30}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without name: %d; want 400 — %s", rec.Code, rec.Body.String())
	}
}

// Two spellings that normalise to the same slug collide, and the second is a 409 like
// any other duplicate — the UNIQUE constraint sees the canonical form.
func TestCreateEventType_duplicateAfterNormaliseIs409(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	mustCreated(t, createET(t, h, apiKey, `{"slug":"intro-call","name":"Intro Call","duration_minutes":30}`), "first")
	rec := createET(t, h, apiKey, `{"slug":"Intro Call","name":"Intro Call","duration_minutes":30}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("create with colliding slug: %d; want 409 — %s", rec.Code, rec.Body.String())
	}
}

// The boot sweep rewrites rows stored before the create path normalised. Oldest first,
// collisions get -2, -3 …, canonical slugs are untouched, and a slug with nothing
// usable in it falls back to the name, then to "event-type".
func TestNormalizeEventTypeSlugs_sweep(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)

	ins := func(id, slug, name, createdAt string) {
		t.Helper()
		mustExec(t, database, `
			INSERT INTO event_types (id, user_id, slug, name, duration_minutes, created_at)
			VALUES (?, ?, ?, ?, 30, ?)`, id, ownerID, slug, name, createdAt)
	}
	ins("canon", "intro-call", "Intro Call", "2024-01-01T00:00:00Z")  // already canonical: keep
	ins("spaced", "Intro Call", "Intro Call", "2024-01-02T00:00:00Z") // → intro-call is taken → intro-call-2
	ins("under", "intro_call", "Intro Call", "2024-01-03T00:00:00Z")  // → intro-call-3
	ins("caps", "DemoCall", "Demo Call", "2024-01-04T00:00:00Z")      // → democall
	ins("junk", "!!!", "Team Sync", "2024-01-05T00:00:00Z")           // nothing usable → from the name
	ins("junk2", "???", "###", "2024-01-06T00:00:00Z")                // name is junk too → fallback
	ins("junk3", "  ", "***", "2024-01-07T00:00:00Z")                 // second fallback → suffixed
	ins("later", "Intro Call ", "Intro Call", "2024-01-08T00:00:00Z") // → intro-call-4

	if err := handler.NormalizeEventTypeSlugs(context.Background(), database, slog.Default()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	want := map[string]string{
		"canon":  "intro-call",
		"spaced": "intro-call-2",
		"under":  "intro-call-3",
		"caps":   "democall",
		"junk":   "team-sync",
		"junk2":  "event-type",
		"junk3":  "event-type-2",
		"later":  "intro-call-4",
	}
	for id, wantSlug := range want {
		var got string
		if err := database.QueryRow(`SELECT slug FROM event_types WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != wantSlug {
			t.Errorf("%s: slug = %q, want %q", id, got, wantSlug)
		}
	}

	// Idempotent: a second run has nothing to do and changes nothing.
	if err := handler.NormalizeEventTypeSlugs(context.Background(), database, slog.Default()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	var got string
	if err := database.QueryRow(`SELECT slug FROM event_types WHERE id = 'spaced'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "intro-call-2" {
		t.Errorf("second sweep moved a slug it had already fixed: %q", got)
	}

	// The rewritten slug resolves on the public booking page; the old one never did.
	req := httptest.NewRequest(http.MethodGet, "/book/intro-call-2", nil)
	req.SetPathValue("slug", "intro-call-2")
	rec := httptest.NewRecorder()
	h.BookPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/book/intro-call-2 after sweep: %d; want 200", rec.Code)
	}
}
