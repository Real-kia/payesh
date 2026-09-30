package fleet

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
)

type nodeControlRequest struct {
	Action                     string `json:"action"`
	Port                       int    `json:"port"`
	User                       string `json:"user"`
	Password                   string `json:"password,omitempty"`
	PrivateKey                 string `json:"private_key,omitempty"`
	PrivateKeyPassphrase       string `json:"private_key_passphrase,omitempty"`
	ExpectedHostKeyFingerprint string `json:"expected_host_key_fingerprint,omitempty"`
}

func (a *API) controlNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	id, ok := installPathServerID(strings.TrimSuffix(r.URL.Path, "/control") + "/install")
	if !ok {
		writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	server, found, err := a.store.GetServer(r.Context(), contracts.ServerID(id))
	if err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "could not read server", true)
		return
	}
	if !found || server.Role != "node" || server.Address == "" {
		writeFleetError(w, http.StatusNotFound, "not_found", "node not found", false)
		return
	}
	var request nodeControlRequest
	if !decode(w, r, &request) {
		return
	}
	if request.Action != "restart" && request.Action != "disable" && request.Action != "enable" || request.Port < 1 || request.Port > 65535 || request.User == "" || request.ExpectedHostKeyFingerprint == "" || (request.Password == "") == (request.PrivateKey == "") {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "invalid node control request", false)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	err = install.ControlNodeOverSSH(ctx, install.SSHEndpoint{Host: server.Address, Port: request.Port, User: request.User}, install.SSHAuth{Password: []byte(request.Password), PrivateKey: []byte(request.PrivateKey), PrivateKeyPassphrase: []byte(request.PrivateKeyPassphrase)}, request.ExpectedHostKeyFingerprint, "", request.Action, nil)
	if err != nil {
		message := "node service action failed"
		if stage := install.SSHInstallFailureStage(err); stage != "" {
			message += " during " + stage
		}
		if detail := install.SSHInstallFailureDetail(err); detail != "" {
			message += ": " + detail
		}
		writeFleetError(w, http.StatusBadGateway, "node_control_failed", message, true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
