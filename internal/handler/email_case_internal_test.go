package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// M5: the OAuth callback finds an account whatever case the provider reports the
// mailbox in, instead of provisioning a second one for the allowed domain.
func TestFinishOAuthLogin_matchesEmailCaseInsensitively(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	for _, q := range []string{
		`UPDATE server_settings SET allowed_signin_domains = 'acme.com' WHERE id = 1`,
		`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('own','Ankur@Acme.com','Owner','UTC',1,1)`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	h := New(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.finishOAuthLogin(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/callback", nil), "ankur@acme.com", "Ankur", true, "")
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("location = %q; want /admin (signed in to the existing account)", loc)
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("users = %d; want 1 (no duplicate provisioned)", n)
	}
	var sessions int
	database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = 'own'`).Scan(&sessions) //nolint:errcheck
	if sessions != 1 {
		t.Errorf("sessions for the owner = %d; want 1", sessions)
	}
}

// M5: setup stores the address lowercased and trimmed, so every later lookup matches.
func TestSetup_lowercasesEmail(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := New(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.Setup(rec, httptest.NewRequest(http.MethodPost, "/v1/setup", strings.NewReader(`{"name":" Ada ","email":"  Ada@Example.COM "}`)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	var email, name string
	database.QueryRow(`SELECT email, name FROM users`).Scan(&email, &name) //nolint:errcheck
	if email != "ada@example.com" || name != "Ada" {
		t.Errorf("stored %q %q; want lowercased, trimmed", email, name)
	}
	// The case-insensitive unique index is in place on a clean database.
	if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone) VALUES ('dup','ADA@example.com','Dup','UTC')`); err == nil {
		t.Error("a second spelling of the same address was accepted; want idx_users_email_nocase to refuse it")
	}
}
