package handler

import (
	"encoding/json"
	"net/http"
	"slices"
)

// API key scopes. A key created without scopes acts with its owner's full role, as every
// key did before scopes existed. A key created with scopes can make only the GET
// requests its scopes list, and nothing else, whatever its owner may do: an admin can
// hand an integration a key that reads every booking (GET /v1/bookings?scope=all) and
// cannot cancel one.
var apiKeyScopeRoutes = map[string][]string{
	"bookings:read": {
		"GET /v1/bookings",
		"GET /v1/bookings/{id}",
		"GET /v1/bookings/{id}/answers",
	},
	"webhooks:read": {
		"GET /v1/webhooks",
		"GET /v1/webhooks/{id}/deliveries",
	},
}

// validAPIKeyScope reports whether s is a scope Calnode knows.
func validAPIKeyScope(s string) bool {
	_, ok := apiKeyScopeRoutes[s]
	return ok
}

// apiKeyAllows reports whether a key with these scopes (JSON array) may serve r. It
// matches the mux pattern that routed the request, so it cannot be fooled by a path
// spelled differently; a request with no pattern (not routed by the mux) is refused.
func apiKeyAllows(scopesJSON string, r *http.Request) bool {
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil || r.Pattern == "" {
		return false
	}
	for _, s := range scopes {
		if slices.Contains(apiKeyScopeRoutes[s], r.Pattern) {
			return true
		}
	}
	return false
}
