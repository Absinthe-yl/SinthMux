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

type AgentHandler struct {
	Registry *devices.Registry
	Manager  *Manager
	DevToken string
}

func (h AgentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.DevToken {
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
	var agent *agentConnection
	defer func() {
		if agent != nil && h.Manager.Unregister(deviceID, agent) {
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
		case protocol.MessageAgentHello:
			if agent != nil || envelope.Version != protocol.Version || envelope.Hello == nil || envelope.Hello.DeviceID == "" {
				_ = writeEnvelope(ctx, connection, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageError, Error: &protocol.Error{Code: "invalid_hello", Message: "deviceId is required"}})
				continue
			}
			deviceID = envelope.Hello.DeviceID
			agent = h.Manager.Register(deviceID, connection)
			h.Registry.Connect(*envelope.Hello)
			_ = agent.send(ctx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageAck, Ack: &protocol.Ack{Message: "agent registered"}})
		case protocol.MessageHeartbeat:
			if agent != nil && envelope.Heartbeat != nil && envelope.Heartbeat.DeviceID == deviceID {
				h.Registry.Touch(deviceID)
			}
		case protocol.MessageRPCResponse:
			if agent == nil || envelope.RequestID == "" {
				continue
			}
			if envelope.Version != protocol.Version || envelope.Response == nil || (envelope.Response.OK && envelope.Response.Error != "") || (!envelope.Response.OK && envelope.Response.Error == "") {
				agent.finish(envelope.RequestID, rpcResult{err: ErrInvalidResponse})
				continue
			}
			agent.finish(envelope.RequestID, rpcResult{response: envelope.Response})
		case protocol.MessageStreamData, protocol.MessageStreamClose:
			if agent != nil && envelope.Version == protocol.Version {
				agent.streamEvent(envelope)
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
