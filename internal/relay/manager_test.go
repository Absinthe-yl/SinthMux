package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func connectedAgent(t *testing.T) (*Manager, *websocket.Conn) {
	t.Helper()
	manager := NewManager()
	server := httptest.NewServer(AgentHandler{Registry: devices.NewRegistry(), Manager: manager, DevToken: "test"})
	t.Cleanup(server.Close)
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer test"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := writeEnvelope(ctx, conn, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageAgentHello, Hello: &protocol.AgentHello{DeviceID: "device-1"}}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.Envelope
	if err := json.Unmarshal(payload, &ack); err != nil || ack.Type != protocol.MessageAck {
		t.Fatalf("bad hello ack: %s, %v", payload, err)
	}
	return manager, conn
}

func readRequest(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var request protocol.Envelope
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != protocol.MessageRPCRequest || request.RequestID == "" || request.Request == nil || request.Request.Method != "tmux.sessions.list" {
		t.Fatalf("bad request: %+v", request)
	}
	return request
}

func TestRPCNormal(t *testing.T) {
	manager, conn := connectedAgent(t)
	result := make(chan *protocol.RPCResponse, 1)
	go func() {
		response, _ := manager.Call(context.Background(), "device-1", protocol.RPCRequest{Method: "tmux.sessions.list"})
		result <- response
	}()
	request := readRequest(t, conn)
	err := writeEnvelope(context.Background(), conn, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: request.RequestID, Response: &protocol.RPCResponse{OK: true, Sessions: []protocol.TmuxSession{{Name: "work", Windows: 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-result:
		if response == nil || len(response.Sessions) != 1 || response.Sessions[0].Name != "work" {
			t.Fatalf("bad response: %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("RPC did not complete")
	}
}

func TestRPCOffline(t *testing.T) {
	manager := NewManager()
	_, err := manager.Call(context.Background(), "missing", protocol.RPCRequest{Method: "tmux.sessions.list"})
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("got %v", err)
	}
}

func TestRPCTimeout(t *testing.T) {
	manager, conn := connectedAgent(t)
	manager.Timeout = 50 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		_, err := manager.Call(context.Background(), "device-1", protocol.RPCRequest{Method: "tmux.sessions.list"})
		result <- err
	}()
	readRequest(t, conn)
	if err := <-result; !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestRPCInvalidResponse(t *testing.T) {
	manager, conn := connectedAgent(t)
	result := make(chan error, 1)
	go func() {
		_, err := manager.Call(context.Background(), "device-1", protocol.RPCRequest{Method: "tmux.sessions.list"})
		result <- err
	}()
	request := readRequest(t, conn)
	if err := writeEnvelope(context.Background(), conn, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("got %v", err)
	}
}

func TestRPCDuplicateRequestID(t *testing.T) {
	manager, conn := connectedAgent(t)
	result := make(chan error, 1)
	go func() {
		_, err := manager.callWithID(context.Background(), "device-1", "same-id", protocol.RPCRequest{Method: "tmux.sessions.list"})
		result <- err
	}()
	readRequest(t, conn)
	_, err := manager.callWithID(context.Background(), "device-1", "same-id", protocol.RPCRequest{Method: "tmux.sessions.list"})
	if !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("got %v", err)
	}
	conn.CloseNow()
	if err := <-result; !errors.Is(err, ErrOffline) {
		t.Fatalf("got %v", err)
	}
}
