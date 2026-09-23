package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/sinthmux/sinthmux/internal/auth"
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
	var authServer *auth.Server
	if settings.DatabaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		store, err := auth.Open(ctx, settings.DatabaseURL)
		cancel()
		if err != nil {
			logger.Error("auth database unavailable", "error", err)
			os.Exit(1)
		}
		defer store.DB.Close()
		if len(os.Args) > 1 && os.Args[1] == "bootstrap-token" {
			name := "Owner"
			if len(os.Args) > 2 {
				name = os.Args[2]
			}
			token, err := store.BootstrapOwner(context.Background(), name)
			if err != nil {
				logger.Error("bootstrap failed; it is allowed only before the first user exists", "error", err)
				os.Exit(1)
			}
			fmt.Println(token)
			return
		}
		public, err := url.Parse(settings.PublicURL)
		if err != nil || public.Host == "" || (public.Scheme != "https" && !(public.Scheme == "http" && (public.Hostname() == "localhost" || public.Hostname() == "127.0.0.1"))) {
			logger.Error("SINTHMUX_PUBLIC_URL must be HTTPS, except on localhost")
			os.Exit(1)
		}
		authServer = auth.NewServer(store, auth.OAuthConfig{ClientID: settings.GithubClientID, ClientSecret: settings.GithubClientSecret, PublicURL: settings.PublicURL, BrokerURL: settings.AuthBrokerURL, BrokerPublicKey: settings.AuthBrokerPublicKey})
		if (settings.AuthBrokerURL != "" || settings.AuthBrokerPublicKey != "") && !authServer.BrokerEnabled() {
			logger.Error("SINTHMUX_AUTH_BROKER_URL and SINTHMUX_AUTH_BROKER_PUBLIC_KEY must form a valid login broker configuration")
			os.Exit(1)
		}
		authServer.OnDeviceRevoked = func(id string) { manager.Revoke(id); registry.Disconnect(id) }
	} else {
		host, _, err := net.SplitHostPort(settings.Address)
		if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			logger.Error("development mode must bind to loopback; set SINTHMUX_DATABASE_URL for formal mode")
			os.Exit(1)
		}
	}

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.Recoverer)
	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "sinthmux-hub", "version": version})
	})
	router.Get("/api/v1/system/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version, "connectedDevices": len(registry.List()), "authMode": map[bool]string{true: "formal", false: "development"}[authServer != nil], "githubLoginEnabled": authServer != nil && authServer.GithubEnabled()})
	})
	sessionAPI := sessionHandler{manager: manager}
	terminalAPI := relay.NewTerminalHandler(manager)
	if authServer != nil {
		public, _ := url.Parse(settings.PublicURL)
		terminalAPI.OriginPattern = public.Host
		terminalAPI.ValidateGrant = func(ctx context.Context, grant relay.TerminalGrant) bool {
			return authServer.Store.TerminalAllowed(ctx, grant.UserID, grant.DeviceID, grant.SpaceID, grant.SessionHash)
		}
		authServer.Mount(router)
	}
	register := func(r chi.Router) {
		r.Get("/api/v1/devices", func(w http.ResponseWriter, request *http.Request) {
			if authServer == nil {
				writeJSON(w, http.StatusOK, map[string]any{"devices": registry.List()})
				return
			}
			session, _ := auth.FromContext(request.Context())
			allowed, err := authServer.Store.Devices(request.Context(), session.User.ID)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": "database error"})
				return
			}
			result := []devices.Device{}
			seen := map[string]bool{}
			for _, device := range registry.List() {
				if registered, ok := allowed[device.ID]; ok {
					device.SpaceID = registered.SpaceID
					device.Name = registered.Name
					result = append(result, device)
					seen[device.ID] = true
				}
			}
			for id, registered := range allowed {
				if !seen[id] {
					result = append(result, devices.Device{ID: id, SpaceID: registered.SpaceID, Name: registered.Name, Status: "offline"})
				}
			}
			writeJSON(w, 200, map[string]any{"devices": result})
		})
		wrap := func(action string, handler http.HandlerFunc) http.HandlerFunc {
			if authServer != nil {
				return authServer.Device(action, func(w http.ResponseWriter, request *http.Request) {
					if action == "list" {
						handler(w, request)
						return
					}
					capture := &statusWriter{ResponseWriter: w, status: 200}
					handler(capture, request)
					session, _ := auth.FromContext(request.Context())
					deviceID := chi.URLParam(request, "deviceId")
					spaceID, _, _ := authServer.Store.DeviceRole(request.Context(), session.User.ID, deviceID)
					result := "ok"
					if capture.status >= 400 {
						result = "error"
					}
					target := deviceID
					if name := chi.URLParam(request, "sessionName"); name != "" {
						target += "/" + name
					}
					authServer.Store.Audit(request.Context(), session.User.ID, spaceID, target, "session."+action, result)
				})
			}
			return handler
		}
		r.Get("/api/v1/devices/{deviceId}/sessions", wrap("list", sessionAPI.list))
		r.Post("/api/v1/devices/{deviceId}/sessions", wrap("create", sessionAPI.create))
		r.Patch("/api/v1/devices/{deviceId}/sessions/{sessionName}", wrap("rename", sessionAPI.rename))
		r.Delete("/api/v1/devices/{deviceId}/sessions/{sessionName}", wrap("close", sessionAPI.close))
		r.Post("/api/v1/devices/{deviceId}/sessions/{sessionName}/ticket", wrap("input", func(w http.ResponseWriter, r *http.Request) {
			deviceID := chi.URLParam(r, "deviceId")
			var grant relay.TerminalGrant
			if authServer != nil {
				session, _ := auth.FromContext(r.Context())
				spaceID, _, _ := authServer.Store.DeviceRole(r.Context(), session.User.ID, deviceID)
				grant = relay.TerminalGrant{UserID: session.User.ID, SpaceID: spaceID, DeviceID: deviceID, SessionHash: session.IDHash}
			}
			ticket, err := terminalAPI.IssueWithGrant(deviceID, chi.URLParam(r, "sessionName"), grant)
			if err != nil {
				status := http.StatusBadRequest
				if err == relay.ErrOffline {
					status = http.StatusServiceUnavailable
				}
				writeJSON(w, status, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
		}))
	}
	if authServer != nil {
		router.Group(func(r chi.Router) { r.Use(authServer.Require); register(r) })
	} else {
		register(router)
	}
	router.Handle("/ws/v1/terminal", terminalAPI)
	connectorHandler := relay.ConnectorHandler{Registry: registry, Manager: manager, DevToken: settings.DevToken}
	if authServer != nil {
		connectorHandler.AuthenticateDevice = func(ctx context.Context, id, authorization string) bool {
			return strings.HasPrefix(authorization, "Bearer ") && authServer.Store.AuthenticateDevice(ctx, id, strings.TrimPrefix(authorization, "Bearer "))
		}
	}
	router.Handle("/ws/v1/connectors/connect", connectorHandler)

	server := &http.Server{Addr: settings.Address, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	logger.Info("SinthMux Hub listening", "address", settings.Address, "version", version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("hub stopped", "error", err)
		os.Exit(1)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
