package fleet

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Real-kia/payesh/internal/auth"
)

type accountRequest struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	Permission string `json:"permission"`
	Password   string `json:"password"`
}

func (a *API) accountHTTP(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(a.sessions.SessionCookieName())
	if err != nil {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	current, ok := a.sessions.SessionAccount(cookie.Value)
	if !ok {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	if r.URL.Path == "/api/v1/account/me" {
		if r.Method != http.MethodGet {
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		writeAccountJSON(w, current)
		return
	}
	if current.Role != "owner" {
		writeFleetError(w, http.StatusForbidden, "owner_required", "owner access required", false)
		return
	}
	if r.URL.Path == "/api/v1/accounts" {
		switch r.Method {
		case http.MethodGet:
			writeAccountJSON(w, struct {
				Items []auth.Account `json:"items"`
			}{a.sessions.Accounts()})
		case http.MethodPost:
			var req accountRequest
			if !decode(w, r, &req) {
				return
			}
			if req.Username == "" || req.Password == "" {
				writeFleetError(w, http.StatusBadRequest, "invalid_account", "username and password are required", false)
				return
			}
			for _, account := range a.sessions.Accounts() {
				if account.Username == req.Username {
					writeFleetError(w, http.StatusConflict, "account_exists", "username already exists", false)
					return
				}
			}
			if err := a.sessions.SaveAccount(req.Username, req.Role, req.Permission, req.Password); err != nil {
				if err.Error() == "username already exists" {
					writeFleetError(w, http.StatusConflict, "account_exists", err.Error(), false)
					return
				}
				writeFleetError(w, http.StatusBadRequest, "invalid_account", err.Error(), false)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		}
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/accounts/")
	if name == "" || strings.Contains(name, "/") {
		writeFleetError(w, http.StatusNotFound, "not_found", "account not found", false)
		return
	}
	var target auth.Account
	found := false
	for _, account := range a.sessions.Accounts() {
		if account.Username == name {
			target, found = account, true
			break
		}
	}
	if !found {
		writeFleetError(w, http.StatusNotFound, "not_found", "account not found", false)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req accountRequest
		if !decode(w, r, &req) {
			return
		}
		role, permission := req.Role, req.Permission
		if target.Role != "owner" {
			if role == "" {
				role = target.Role
			}
			if permission == "" {
				permission = target.Permission
			}
		}
		if err := a.sessions.UpdateAccount(name, req.Username, role, permission, req.Password); err != nil {
			writeFleetError(w, http.StatusBadRequest, "invalid_account", err.Error(), false)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := a.sessions.DeleteAccount(name); err != nil {
			writeFleetError(w, http.StatusBadRequest, "invalid_account", err.Error(), false)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
	}
}

func writeAccountJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
