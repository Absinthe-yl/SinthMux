package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestSessionAPI(t *testing.T) {
	manager := relay.NewManager()
	router := chi.NewRouter()
	api := sessionHandler{manager: manager}
	router.Get("/api/v1/devices/{deviceId}/sessions", api.list)
	router.Post("/api/v1/devices/{deviceId}/sessions", api.create)
	router.Patch("/api/v1/devices/{deviceId}/sessions/{sessionName}", api.rename)
	router.Delete("/api/v1/devices/{deviceId}/sessions/{sessionName}", api.close)
	router.Handle("/ws", relay.ConnectorHandler{Registry: devices.NewRegistry(), Manager: manager, DevToken: "test"})
	server := httptest.NewServer(router)
	defer server.Close()

	base := server.URL + "/api/v1/devices/test-device/sessions"
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	assertStatus := func(response *http.Response, want int) {
		t.Helper()
		defer response.Body.Close()
		if response.StatusCode != want {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("status %d, want %d: %s", response.StatusCode, want, body)
		}
	}
	assertStatus(request(http.MethodPost, base, `{"name":"bad.name"}`), http.StatusBadRequest)
	assertStatus(request(http.MethodPost, base, `{"name":"work"}`), http.StatusServiceUnavailable)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	write := func(envelope protocol.Envelope) {
		t.Helper()
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
	}
	write(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: "test-device"}})
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		method, path, body, rpcMethod, name, newName string
		status                                       int
		response                                     protocol.RPCResponse
	}{
		{http.MethodPost, base, `{"name":"work"}`, "tmux.sessions.create", "work", "", http.StatusCreated, protocol.RPCResponse{OK: true, Name: "work"}},
		{http.MethodPatch, base + "/work", `{"name":"next"}`, "tmux.sessions.rename", "work", "next", http.StatusOK, protocol.RPCResponse{OK: true, Name: "next"}},
		{http.MethodDelete, base + "/next", "", "tmux.sessions.close", "next", "", http.StatusNoContent, protocol.RPCResponse{OK: true}},
		{http.MethodPost, base, `{"name":"next"}`, "tmux.sessions.create", "next", "", http.StatusConflict, protocol.RPCResponse{ErrorCode: "already_exists", Error: "session already exists"}},
	} {
		result := make(chan *http.Response, 1)
		go func() {
			req, _ := http.NewRequest(step.method, step.path, strings.NewReader(step.body))
			response, _ := server.Client().Do(req)
			result <- response
		}()
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var envelope protocol.Envelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Request == nil || envelope.Request.Method != step.rpcMethod || envelope.Request.Name != step.name || envelope.Request.NewName != step.newName {
			t.Fatalf("RPC request: %+v", envelope)
		}
		write(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: envelope.RequestID, Response: &step.response})
		assertStatus(<-result, step.status)
	}
}
