package fleet

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/auth"
)

const defaultTokenLifetimeDays = 90

type apiTokenRequest struct {
	Name          string   `json:"name"`
	Permission    string   `json:"permission"`
	ExpiresInDays int      `json:"expires_in_days"`
	ServerIDs     []string `json:"server_ids"`
	Actions       []string `json:"actions"`
}

type createdAPIToken struct {
	auth.APIToken
	// Token is the bearer secret. It is returned only in this response.
	Token string `json:"token"`
}

func tokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrTokenNotFound):
		writeFleetError(w, http.StatusNotFound, "not_found", "token not found", false)
	case errors.Is(err, auth.ErrTokenPersistence):
		writeFleetError(w, http.StatusInternalServerError, "persistence_failed", "unable to save token changes", true)
	default:
		writeFleetError(w, http.StatusBadRequest, "invalid_token", err.Error(), false)
	}
}

func validTokenDays(w http.ResponseWriter, days int, allowDefault bool) bool {
	if (allowDefault && days == 0) || (days >= 1 && days <= 365) {
		return true
	}
	writeFleetError(w, http.StatusBadRequest, "invalid_token", "expires_in_days must be between 1 and 365", false)
	return false
}

// apiTokenHTTP manages tokens from browser sessions. The owner may list,
// inspect, and revoke other accounts' tokens, but only a token's own account
// can rotate or edit it: a rotation hands out a working secret, and the owner
// must not obtain a credential that acts as another user. Bearer credentials
// cannot manage credentials.
func (a *API) apiTokenHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := a.sessions.Authenticate(r)
	if !ok {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	if principal.TokenID != "" {
		writeFleetError(w, http.StatusForbidden, "session_required", "API tokens are managed from a signed-in browser session", false)
		return
	}
	scope := principal.Username
	if principal.Role == "owner" {
		scope = ""
	}
	if r.URL.Path == "/api/v1/api-tokens" {
		username := strings.TrimSpace(r.URL.Query().Get("username"))
		if username != "" && principal.Role != "owner" && username != principal.Username {
			writeFleetError(w, http.StatusForbidden, "forbidden", "cannot manage another account's tokens", false)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if username != "" {
				scope = username
			}
			search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
			tokens := a.sessions.Tokens(scope)
			items := make([]auth.APIToken, 0, len(tokens))
			for _, token := range tokens {
				if search == "" || strings.Contains(strings.ToLower(token.Name+" "+token.Username+" "+token.Hint), search) {
					items = append(items, token)
				}
			}
			writeAccountJSON(w, struct {
				Items []auth.APIToken `json:"items"`
			}{items})
		case http.MethodPost:
			var request apiTokenRequest
			if !decode(w, r, &request) {
				return
			}
			if request.Permission == "" {
				request.Permission = "read"
			}
			if !validTokenDays(w, request.ExpiresInDays, true) {
				return
			}
			if request.ExpiresInDays == 0 {
				request.ExpiresInDays = defaultTokenLifetimeDays
			}
			secret, token, err := a.sessions.CreateScopedToken(principal.Username, request.Name, request.Permission, time.Duration(request.ExpiresInDays)*24*time.Hour, auth.TokenOptions{ServerIDs: request.ServerIDs, Actions: request.Actions})
			if err != nil {
				tokenError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			writeAccountJSON(w, createdAPIToken{APIToken: token, Token: secret})
		case http.MethodDelete:
			if username == "" {
				if principal.Role == "owner" {
					writeFleetError(w, http.StatusBadRequest, "invalid_request", "username is required to revoke all tokens", false)
					return
				}
				username = principal.Username
			}
			count, err := a.sessions.RevokeAllTokens(username)
			if err != nil {
				tokenError(w, err)
				return
			}
			writeAccountJSON(w, struct {
				Revoked int `json:"revoked"`
			}{count})
		default:
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/api-tokens/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		writeFleetError(w, http.StatusNotFound, "not_found", "token not found", false)
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		switch parts[1] {
		case "rotate":
			if r.Method != http.MethodPost {
				writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
				return
			}
			var request struct {
				ExpiresInDays int `json:"expires_in_days"`
			}
			// An omitted body preserves the current expiry; {} does the same.
			if r.Body != nil && r.ContentLength != 0 && !decode(w, r, &request) {
				return
			}
			if !validTokenDays(w, request.ExpiresInDays, true) {
				return
			}
			secret, token, err := a.sessions.RotateToken(principal.Username, id, request.ExpiresInDays)
			if err != nil {
				tokenError(w, err)
				return
			}
			writeAccountJSON(w, createdAPIToken{APIToken: token, Token: secret})
		case "activity":
			if r.Method != http.MethodGet {
				writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
				return
			}
			activity, err := a.sessions.TokenActivity(scope, id)
			if err != nil {
				tokenError(w, err)
				return
			}
			writeAccountJSON(w, struct {
				Items []auth.TokenActivity `json:"items"`
			}{activity})
		default:
			writeFleetError(w, http.StatusNotFound, "not_found", "token not found", false)
		}
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := a.sessions.RevokeToken(scope, id); err != nil {
			tokenError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		var request auth.TokenUpdate
		if !decode(w, r, &request) {
			return
		}
		if request.ExpiresInDays != nil && !validTokenDays(w, *request.ExpiresInDays, false) {
			return
		}
		token, err := a.sessions.UpdateToken(principal.Username, id, request)
		if err != nil {
			tokenError(w, err)
			return
		}
		writeAccountJSON(w, token)
	default:
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
	}
}
