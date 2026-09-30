package handler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

func autoJoinTestHandler(t *testing.T, domains string) *Handler {
	t.Helper()
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(
		`UPDATE server_settings SET google_auto_join_domains = ? WHERE id = 1`, domains); err != nil {
		t.Fatalf("seed domains: %v", err)
	}
	return New(database, slog.Default())
}

func countUsers(t *testing.T, h *Handler, email string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = ?`, email).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestNormalizeAutoJoinDomains(t *testing.T) {
	got, err := normalizeAutoJoinDomains([]string{" Example.COM ", "example.com", "", "sub.example.org"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "example.com" || got[1] != "sub.example.org" {
		t.Errorf("got %v; want [example.com sub.example.org]", got)
	}
	for _, bad := range []string{"someone@example.com", "https://example.com", "example", ".example.com", "example..com", "ex ample.com"} {
		if _, err := normalizeAutoJoinDomains([]string{bad}); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// The gate is Google's hosted-domain claim, not the email text: a personal Gmail whose
// address merely mentions the domain, and a Workspace user on a different domain, are
// both refused; only a matching hd creates a member.
func TestAutoProvisionGoogleUser_gatesOnHostedDomain(t *testing.T) {
	h := autoJoinTestHandler(t, "example.com,other.org")
	ctx := context.Background()

	cases := []struct {
		name string
		info googleUserInfo
		want bool
	}{
		{"matching hd", googleUserInfo{Email: "ana@example.com", Name: "Ana", VerifiedEmail: true, HD: "example.com"}, true},
		{"hd is case-insensitive", googleUserInfo{Email: "bo@example.com", VerifiedEmail: true, HD: "Example.COM"}, true},
		{"second listed domain", googleUserInfo{Email: "cy@other.org", VerifiedEmail: true, HD: "other.org"}, true},
		{"no hd (personal gmail)", googleUserInfo{Email: "example.com@gmail.com", VerifiedEmail: true}, false},
		{"hd not listed", googleUserInfo{Email: "di@evil.com", VerifiedEmail: true, HD: "evil.com"}, false},
		{"email domain matches but hd does not", googleUserInfo{Email: "spoof@example.com", VerifiedEmail: true, HD: "evil.com"}, false},
		{"unverified", googleUserInfo{Email: "ed@example.com", VerifiedEmail: false, HD: "example.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := h.autoProvisionGoogleUser(ctx, &tc.info, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if created != tc.want {
				t.Errorf("created = %v; want %v", created, tc.want)
			}
			if n := countUsers(t, h, tc.info.Email); (n == 1) != tc.want {
				t.Errorf("user rows = %d; want created=%v", n, tc.want)
			}
		})
	}

	// The provisioned member is a plain, password-less, non-admin Google user.
	var isAdmin, emailLogin int
	var provider, name string
	if err := h.db.QueryRow(`SELECT is_admin, email_login, provider, name FROM users WHERE email = 'bo@example.com'`).
		Scan(&isAdmin, &emailLogin, &provider, &name); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if isAdmin != 0 || emailLogin != 0 || provider != "google" || name != "bo" {
		t.Errorf("got is_admin=%d email_login=%d provider=%q name=%q", isAdmin, emailLogin, provider, name)
	}
}

func TestAutoProvisionGoogleUser_offByDefaultAndIdempotent(t *testing.T) {
	h := autoJoinTestHandler(t, "")
	ctx := context.Background()
	info := &googleUserInfo{Email: "ana@example.com", VerifiedEmail: true, HD: "example.com"}
	if created, err := h.autoProvisionGoogleUser(ctx, info, ""); err != nil || created {
		t.Fatalf("empty allow-list: created=%v err=%v; want false, nil", created, err)
	}

	h2 := autoJoinTestHandler(t, "example.com")
	if created, _ := h2.autoProvisionGoogleUser(ctx, info, "Asia/Kolkata"); !created {
		t.Fatal("first login should create the user")
	}
	if created, err := h2.autoProvisionGoogleUser(ctx, info, ""); err != nil || created {
		t.Fatalf("second login: created=%v err=%v; want false, nil", created, err)
	}
	if n := countUsers(t, h2, info.Email); n != 1 {
		t.Errorf("user rows = %d; want 1", n)
	}
	var tz string
	h2.db.QueryRow(`SELECT iana_timezone FROM users WHERE email = ?`, info.Email).Scan(&tz)
	if tz != "Asia/Kolkata" {
		t.Errorf("iana_timezone = %q; want the browser zone passed at first login", tz)
	}
}
