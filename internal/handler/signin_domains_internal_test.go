package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

func TestParseSigninDomains_normalisesAndValidates(t *testing.T) {
	got, err := parseSigninDomains([]string{" @Acme.com, acme.co.uk\nexample.org;ACME.COM ", "", "  ", "@"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme.co.uk", "acme.com", "example.org"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
	for _, bad := range []string{"acme", "http://acme.com", "acme.com/x", "a b.com", "-acme.com", "user@acme.com"} {
		if _, err := parseSigninDomains([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		} else if err.Error() == "" {
			t.Errorf("%q: error should name the entry", bad)
		}
	}
	// "user@acme.com" is rejected because only a leading "@" is stripped.
	if _, err := parseSigninDomains([]string{"user@acme.com"}); err == nil || err.Error() != `"user@acme.com" is not a valid domain` {
		t.Errorf("error = %v", err)
	}
	if got, err := parseSigninDomains(nil); err != nil || len(got) != 0 {
		t.Errorf("nil → %v, %v", got, err)
	}
}

func TestSigninDomainAllowed_exactDomainOnly(t *testing.T) {
	domains := []string{"acme.com"}
	for email, want := range map[string]bool{
		"a@acme.com":      true,
		"A@ACME.COM":      true,
		"a@sub.acme.com":  false,
		"a@notacme.com":   false,
		"acme.com":        false,
		"a@":              false,
		"":                false,
		"x@y@acme.com":    true,
		"a@acme.com.evil": false,
	} {
		if got := signinDomainAllowed(email, domains); got != want {
			t.Errorf("%q: got %v, want %v", email, got, want)
		}
	}
	if signinDomainAllowed("a@acme.com", nil) {
		t.Error("no domains → nothing allowed")
	}
}

// Google refuses unverified addresses before finishOAuthLogin runs, but the gate is
// finishOAuthLogin's own: a provider that passes verified=false for an unknown address
// on an allowed domain must not mint a member.
func TestFinishOAuthLogin_unverifiedNeverProvisions(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(`UPDATE server_settings SET allowed_signin_domains = 'acme.com' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	h := New(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/callback", nil)
	rec := httptest.NewRecorder()
	h.finishOAuthLogin(rec, req, "new@acme.com", "New", false)
	if loc := rec.Header().Get("Location"); loc != "/admin/login?error=no_account" {
		t.Errorf("location = %q; want no_account", loc)
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("users = %d; want 0", n)
	}
}
