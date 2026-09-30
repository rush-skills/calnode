package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/calnode/calnode/internal/uid"
)

// Google auto-join: members of an allow-listed Google Workspace domain get an account
// on their first Google sign-in instead of the "no_account" redirect. The gate is the
// `hd` (hosted domain) claim Google sets only for Workspace accounts, so a personal
// Gmail address such as someone@gmail.com, or a Workspace user on a different domain,
// never matches even when the email text looks right. Email is still required to be
// verified by fetchGoogleUserInfo before we get here.

const maxAutoJoinDomains = 20

// parseAutoJoinDomains splits the stored comma-separated column. It trusts the value
// (normalizeAutoJoinDomains ran on the way in) and only drops empties.
func parseAutoJoinDomains(raw string) []string {
	var out []string
	for _, d := range strings.Split(raw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// normalizeAutoJoinDomains lowercases, trims, dedupes and validates an admin-supplied
// list. It rejects anything that is not a bare hostname with a dot in it: an "@" means
// someone pasted an address, a scheme or slash means they pasted a URL, and a dotless
// word is never a Workspace domain. The returned slice is what gets stored.
func normalizeAutoJoinDomains(in []string) ([]string, error) {
	if len(in) > maxAutoJoinDomains {
		return nil, fmt.Errorf("at most %d domains", maxAutoJoinDomains)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, "@/:, \t") || !strings.Contains(d, ".") ||
			strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") || strings.Contains(d, "..") {
			return nil, fmt.Errorf("%q is not a valid domain (expected e.g. example.com)", raw)
		}
		if len(d) > 253 {
			return nil, fmt.Errorf("%q is too long", raw)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// loadAutoJoinDomains reads the allow-list. A missing settings row means "off".
func (h *Handler) loadAutoJoinDomains(ctx context.Context) ([]string, error) {
	var raw string
	err := h.db.QueryRowContext(ctx,
		`SELECT google_auto_join_domains FROM server_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseAutoJoinDomains(raw), nil
}

// autoProvisionGoogleUser creates a regular (non-admin, no password) member for a
// Google sign-in when no account exists for the email and the account's hosted domain
// is allow-listed. It returns true when a user was created. A false with nil error means
// the caller should carry on with the ordinary lookup: either the user already exists,
// or the domain is not allowed (the normal no_account outcome). An email race (two first
// logins at once) is absorbed: the UNIQUE(email) conflict is ignored and the lookup that
// follows finds the row the other request inserted. tz is the browser's IANA zone when
// the login page supplied one (already validated by browserTimezone); empty falls back
// to UTC, which the member can change under Settings → Profile.
func (h *Handler) autoProvisionGoogleUser(ctx context.Context, info *googleUserInfo, tz string) (bool, error) {
	hd := strings.ToLower(strings.TrimSpace(info.HD))
	if hd == "" || !info.VerifiedEmail || info.Email == "" {
		return false, nil
	}
	var exists int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE email = ?`, info.Email).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	domains, err := h.loadAutoJoinDomains(ctx)
	if err != nil {
		return false, err
	}
	allowed := false
	for _, d := range domains {
		if d == hd {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}
	name := strings.TrimSpace(info.Name)
	if name == "" {
		name = info.Email
		if at := strings.IndexByte(name, '@'); at > 0 {
			name = name[:at]
		}
	}
	if tz == "" {
		tz = "UTC"
	}
	res, err := h.db.ExecContext(ctx, `
		INSERT INTO users (id, email, name, iana_timezone, is_admin, email_login, provider)
		VALUES (?, ?, ?, ?, 0, 0, 'google')
		ON CONFLICT(email) DO NOTHING`,
		uid.New(), info.Email, name, tz)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
