package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
)

const version = "0.0.1-dev"

func main() {
	settings := config.HubFromEnv()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	registry := devices.NewRegistry()
	manager := relay.NewManager()

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "sinthmux-hub", "version": version})
	})
	router.Get("/api/v1/system/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version, "connectedDevices": len(registry.List())})
	})
	router.Get("/api/v1/devices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"devices": registry.List()})
	})
	sessionAPI := sessionHandler{manager: manager}
	terminalAPI := relay.NewTerminalHandler(manager)
	router.Get("/api/v1/devices/{deviceId}/sessions", sessionAPI.list)
	router.Post("/api/v1/devices/{deviceId}/sessions", sessionAPI.create)
	router.Patch("/api/v1/devices/{deviceId}/sessions/{sessionName}", sessionAPI.rename)
	router.Delete("/api/v1/devices/{deviceId}/sessions/{sessionName}", sessionAPI.close)
	router.Post("/api/v1/devices/{deviceId}/sessions/{sessionName}/ticket", func(w http.ResponseWriter, r *http.Request) {
		ticket, err := terminalAPI.Issue(chi.URLParam(r, "deviceId"), chi.URLParam(r, "sessionName"))
		if err != nil {
			status := http.StatusBadRequest
			if err == relay.ErrOffline {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
	})
	router.Handle("/ws/v1/terminal", terminalAPI)
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
