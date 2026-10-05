package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	sessionCookieName = "calnode_session"
	stateCookieName   = "calnode_oauth_state"
	// tzCookieName carries the browser's IANA timezone (from ?tz= on the OAuth login
	// URLs) across the provider round-trip so a member auto-provisioned from an allowed
	// sign-in domain gets their availability seeded in their own zone rather than UTC.
	// Same lifetime as the state cookie; only read when a user is being created.
	tzCookieName    = "calnode_oauth_tz"
	sessionDuration = 30 * 24 * time.Hour
	stateDuration   = 5 * time.Minute
)

// SetGoogleAuth configures the handler for Google OAuth sign-in.
// Called from server.New when GOOGLE_CLIENT_ID is set.
// secure should be true when BASE_URL starts with https://.
func (h *Handler) SetGoogleAuth(clientID, clientSecret, redirectURL string, secure bool) {
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile"},
	}
	h.authMu.Lock()
	h.googleAuth = cfg
	h.secureCookie = secure
	h.authMu.Unlock()
}

// LoginGoogle redirects the user to Google's OAuth consent screen.
// GET /v1/auth/login
func (h *Handler) LoginGoogle(w http.ResponseWriter, r *http.Request) {
	ga := h.getGoogleAuth()
	if ga == nil {
		http.Error(w, "Google OAuth not configured — set GOOGLE_CLIENT_ID", http.StatusServiceUnavailable)
		return
	}
	state, err := h.newOAuthState(w)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: generate state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.rememberBrowserTimezone(w, r)
	http.Redirect(w, r, ga.AuthCodeURL(state, oauth2.AccessTypeOnline), http.StatusFound)
}

// rememberBrowserTimezone stores a valid ?tz= IANA zone in a short-lived cookie for
// browserTimezone to read on the callback. Invalid or absent values set nothing.
func (h *Handler) rememberBrowserTimezone(w http.ResponseWriter, r *http.Request) {
	tz := r.URL.Query().Get("tz")
	if tz == "" || tz == "UTC" || len(tz) > 64 {
		return
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     tzCookieName,
		Value:    tz,
		Path:     "/",
		MaxAge:   int(stateDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
}

// browserTimezone returns the IANA zone the login page sent along, or "" when absent
// or invalid, and clears the cookie either way (single use, like the state cookie).
func (h *Handler) browserTimezone(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(tzCookieName)
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     tzCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
	if err != nil || c.Value == "" {
		return ""
	}
	if _, err := time.LoadLocation(c.Value); err != nil {
		return ""
	}
	return c.Value
}

// CallbackGoogle handles the OAuth redirect from Google.
// GET /v1/auth/callback
func (h *Handler) CallbackGoogle(w http.ResponseWriter, r *http.Request) {
	ga := h.getGoogleAuth()
	if ga == nil {
		http.Error(w, "Google OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	// Verify CSRF state (cookie must match the URL param) and consume it.
	if !h.verifyOAuthState(w, r) {
		http.Redirect(w, r, "/admin/login?error=state", http.StatusFound)
		return
	}

	if r.URL.Query().Get("error") != "" {
		// User denied consent.
		http.Redirect(w, r, "/admin/login?error=denied", http.StatusFound)
		return
	}

	tok, err := ga.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: token exchange", "error", err)
		http.Redirect(w, r, "/admin/login?error=oauth", http.StatusFound)
		return
	}

	info, err := fetchGoogleUserInfo(r.Context(), ga, tok)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "auth: user info", "error", err)
		http.Redirect(w, r, "/admin/login?error=userinfo", http.StatusFound)
		return
	}

	// Known emails sign in; an unknown one only via an allowed sign-in domain, and
	// only when Google says the address is verified — an unverified Google account
	// can claim any address, so it must never mint a member for it.
	h.finishOAuthLogin(w, r, info.Email, info.Name, info.VerifiedEmail, h.browserTimezone(w, r))
}

// Logout deletes the session record and clears the session cookie.
// POST /v1/auth/logout
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// Only delete the session if the cookie value corresponds to an actual row,
	// so a forged or empty cookie cannot be used to trigger arbitrary deletes.
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		// Best-effort: logout must proceed (cookie is cleared below) even if this fails;
		// worst case is a harmless stale row that the session's own expiry cleans up.
		//nolint:errcheck
		// #nosec G104
		h.db.ExecContext(r.Context(),
			`DELETE FROM sessions WHERE id = ?`, cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

type googleUserInfo struct {
	Email         string `json:"email"`
	Name          string `json:"name"`
	VerifiedEmail bool   `json:"verified_email"`
}

func fetchGoogleUserInfo(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (*googleUserInfo, error) {
	resp, err := cfg.Client(ctx, tok).Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, fmt.Errorf("auth: userinfo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: userinfo status %d", resp.StatusCode)
	}
	var info googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("auth: decode userinfo: %w", err)
	}
	if info.Email == "" {
		return nil, fmt.Errorf("auth: empty email from Google")
	}
	// Reject unverified emails: an attacker could claim an unverified address
	// matching a real user's email and bypass the user-lookup check.
	if !info.VerifiedEmail {
		return nil, fmt.Errorf("auth: Google email not verified")
	}
	return &info, nil
}
