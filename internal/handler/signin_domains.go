package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/calnode/calnode/internal/uid"
)

// Allowed sign-in domains: an org can let anyone with a verified Google or Microsoft
// account under its own domain(s) sign in and become a member on first login, instead
// of inviting each person. The list is the ONLY thing that turns self-registration on;
// empty keeps the historical invite-only behaviour. Auto-created users are plain
// members (never admin), with password login off, so the identity provider stays the
// authority for who is in the org.

// reDomain accepts a hostname-shaped domain: labels of letters, digits and hyphens,
// at least one dot, no scheme, path or '@'.
var reDomain = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// errBadDomain names the first entry that is not a domain.
type errBadDomain struct{ entry string }

func (e errBadDomain) Error() string { return fmt.Sprintf("%q is not a valid domain", e.entry) }

// parseSigninDomains normalises an admin's entry list: split on commas, whitespace and
// newlines, lowercase, strip a leading "@" (people paste "@acme.com"), dedupe, sort.
// Returns an error naming the first entry that is not a domain, so the UI can show it.
func parseSigninDomains(entries []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		for _, part := range strings.FieldsFunc(e, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
			d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), "@"))
			if d == "" {
				continue
			}
			if !reDomain.MatchString(d) {
				return nil, errBadDomain{part}
			}
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// signinDomainAllowed reports whether email's domain is in domains (exact match on
// the part after the last "@"; subdomains are not implied, an org lists each one).
func signinDomainAllowed(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	d := strings.ToLower(email[at+1:])
	for _, allowed := range domains {
		if d == allowed {
			return true
		}
	}
	return false
}

// allowedSigninDomains reads the stored list. A missing settings row reads as empty.
func (h *Handler) allowedSigninDomains(ctx context.Context) ([]string, error) {
	var raw string
	err := h.db.QueryRowContext(ctx, `SELECT allowed_signin_domains FROM server_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Stored values were validated on save; a malformed one (edited by hand) is dropped
	// rather than letting a garbage entry match anything.
	domains, _ := parseSigninDomains([]string{raw})
	return domains, nil
}

// provisionOAuthUser creates the member account for a first sign-in from an allowed
// domain. name falls back to the address's local part so the row is never blank; the
// person can change it in their profile. Returns the new user id.
func (h *Handler) provisionOAuthUser(ctx context.Context, email, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = email[:strings.LastIndex(email, "@")]
	}
	id := uid.New()
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO users (id, email, name, iana_timezone, is_admin, email_login)
		VALUES (?, ?, ?, 'UTC', 0, 0)`, id, email, name); err != nil {
		return "", err
	}
	h.logger.InfoContext(ctx, "auth: member auto-provisioned from allowed sign-in domain", "user_id", id, "email", email)
	return id, nil
}

// GetSigninSettings handles GET /v1/settings/signin (admin only).
func (h *Handler) GetSigninSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	domains, err := h.allowedSigninDomains(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "signin settings: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if domains == nil {
		domains = []string{}
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"allowed_signin_domains": domains})
}

// PatchSigninSettings handles PATCH /v1/settings/signin (admin only).
// Body: {"allowed_signin_domains": ["acme.com", "acme.co.uk"]}. An empty list turns
// auto-provisioning off. Omitting the field leaves it unchanged.
func (h *Handler) PatchSigninSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Domains *[]string `json:"allowed_signin_domains"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Domains != nil {
		domains, err := parseSigninDomains(*req.Domains)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.db.ExecContext(r.Context(), `
			UPDATE server_settings SET allowed_signin_domains = ?, updated_at = datetime('now') WHERE id = 1`,
			strings.Join(domains, ",")); err != nil {
			h.logger.ErrorContext(r.Context(), "signin settings: update", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	h.GetSigninSettings(w, r)
}
