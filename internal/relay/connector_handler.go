package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type ConnectorHandler struct {
	Registry           *devices.Registry
	Manager            *Manager
	DevToken           string
	AuthenticateDevice func(context.Context, string, string) bool
}

func (h ConnectorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authorizedID := ""
	if h.AuthenticateDevice != nil {
		authorizedID = r.Header.Get("X-Sinthmux-Device-ID")
		if authorizedID == "" || !h.AuthenticateDevice(r.Context(), authorizedID, r.Header.Get("Authorization")) {
			http.Error(w, "invalid device credential", http.StatusUnauthorized)
			return
		}
	} else if r.Header.Get("Authorization") != "Bearer "+h.DevToken {
		http.Error(w, "invalid development token", http.StatusUnauthorized)
		return
	}

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"localhost:*", "127.0.0.1:*"}})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(64 << 10)

	ctx := r.Context()
	var deviceID string
	var connector *connectorConnection
	defer func() {
		if connector != nil && h.Manager.Unregister(deviceID, connector) {
			h.Registry.Disconnect(deviceID)
		}
	}()

	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}

		var envelope protocol.Envelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			_ = writeEnvelope(ctx, connection, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageError, Error: &protocol.Error{Code: "invalid_message", Message: err.Error()}})
			continue
		}

		switch envelope.Type {
		case protocol.MessageConnectorHello:
			if connector != nil || envelope.Version != protocol.Version || envelope.Hello == nil || envelope.Hello.DeviceID == "" || (authorizedID != "" && envelope.Hello.DeviceID != authorizedID) {
				_ = writeEnvelope(ctx, connection, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageError, Error: &protocol.Error{Code: "invalid_hello", Message: "deviceId is required"}})
				return
			}
			deviceID = envelope.Hello.DeviceID
			connector = h.Manager.Register(deviceID, connection)
			h.Registry.Connect(*envelope.Hello)
			_ = connector.send(ctx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageAck, Ack: &protocol.Ack{Message: "connector registered"}})
		case protocol.MessageHeartbeat:
			if connector != nil && envelope.Heartbeat != nil && envelope.Heartbeat.DeviceID == deviceID {
				h.Registry.Touch(deviceID)
			}
		case protocol.MessageRPCResponse:
			if connector == nil || envelope.RequestID == "" {
				continue
			}
			if envelope.Version != protocol.Version || envelope.Response == nil || (envelope.Response.OK && envelope.Response.Error != "") || (!envelope.Response.OK && envelope.Response.Error == "") {
				connector.finish(envelope.RequestID, rpcResult{err: ErrInvalidResponse})
				continue
			}
			connector.finish(envelope.RequestID, rpcResult{response: envelope.Response})
		case protocol.MessageStreamData, protocol.MessageStreamClose:
			if connector != nil && envelope.Version == protocol.Version {
				connector.streamEvent(envelope)
			}
		default:
			_ = writeEnvelope(ctx, connection, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageError, Error: &protocol.Error{Code: "unsupported_message", Message: string(envelope.Type)}})
		}
	}
}

func writeEnvelope(parent context.Context, connection *websocket.Conn, envelope protocol.Envelope) error {
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
