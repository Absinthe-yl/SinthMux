package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type sessionHandler struct{ manager *relay.Manager }

func (h sessionHandler) call(w http.ResponseWriter, r *http.Request, request protocol.RPCRequest) (*protocol.RPCResponse, bool) {
	response, err := h.manager.Call(r.Context(), chi.URLParam(r, "deviceId"), request)
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, relay.ErrOffline):
			status = http.StatusServiceUnavailable
		case errors.Is(err, relay.ErrTimeout):
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return nil, false
	}
	if !response.OK {
		status := http.StatusBadGateway
		switch response.ErrorCode {
		case "invalid_name", "invalid_request":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "already_exists":
			status = http.StatusConflict
		case "tmux_unavailable":
			status = http.StatusServiceUnavailable
		case "timeout":
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]string{"error": response.Error})
		return nil, false
	}
	return response, true
}

func (h sessionHandler) list(w http.ResponseWriter, r *http.Request) {
	response, ok := h.call(w, r, protocol.RPCRequest{Method: "tmux.sessions.list"})
	if !ok {
		return
	}
	sessions := response.Sessions
	if sessions == nil {
		sessions = []protocol.TmuxSession{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func readSessionName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return "", false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON object"})
		return "", false
	}
	if !protocol.ValidSessionName(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session name must start with a letter or digit and contain only letters, digits, underscores or hyphens (max 64 characters)"})
		return "", false
	}
	return body.Name, true
}

func (h sessionHandler) create(w http.ResponseWriter, r *http.Request) {
	name, ok := readSessionName(w, r)
	if !ok {
		return
	}
	if _, ok := h.call(w, r, protocol.RPCRequest{Method: "tmux.sessions.create", Name: name}); !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": name})
}

func (h sessionHandler) rename(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "sessionName")
	if !protocol.ValidSessionName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session name"})
		return
	}
	newName, ok := readSessionName(w, r)
	if !ok {
		return
	}
	if _, ok := h.call(w, r, protocol.RPCRequest{Method: "tmux.sessions.rename", Name: name, NewName: newName}); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": newName})
}

func (h sessionHandler) close(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "sessionName")
	if !protocol.ValidSessionName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session name"})
		return
	}
	if _, ok := h.call(w, r, protocol.RPCRequest{Method: "tmux.sessions.close", Name: name}); !ok {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
