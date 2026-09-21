package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

const version = "0.0.1-dev"

func main() {
	settings := config.HubFromEnv()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	registry := devices.NewRegistry()
	manager := relay.NewManager()

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(30*time.Second))
	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "sinthmux-hub", "version": version})
	})
	router.Get("/api/v1/system/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version, "connectedDevices": len(registry.List())})
	})
	router.Get("/api/v1/devices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"devices": registry.List()})
	})
	router.Get("/api/v1/devices/{deviceId}/sessions", func(w http.ResponseWriter, r *http.Request) {
		response, err := manager.Call(r.Context(), chi.URLParam(r, "deviceId"), "tmux.sessions.list")
		if err != nil {
			status := http.StatusBadGateway
			switch {
			case errors.Is(err, relay.ErrOffline):
				status = http.StatusServiceUnavailable
			case errors.Is(err, relay.ErrTimeout):
				status = http.StatusGatewayTimeout
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		if !response.OK {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": response.Error})
			return
		}
		sessions := response.Sessions
		if sessions == nil {
			sessions = []protocol.TmuxSession{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	})
	router.Handle("/ws/v1/agents/connect", relay.AgentHandler{Registry: registry, Manager: manager, DevToken: settings.DevToken})

	server := &http.Server{Addr: settings.Address, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	logger.Info("SinthMux Hub listening", "address", settings.Address, "version", version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("hub stopped", "error", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
