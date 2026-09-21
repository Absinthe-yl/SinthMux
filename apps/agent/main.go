package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

const version = "0.0.1-dev"

func main() {
	settings := config.AgentFromEnv()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	backoff := time.Second

	for {
		if err := run(context.Background(), settings, logger); err != nil {
			logger.Warn("agent disconnected", "error", err, "retryIn", backoff)
			time.Sleep(backoff)
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func run(ctx context.Context, settings config.Agent, logger *slog.Logger) error {
	headers := http.Header{"Authorization": []string{"Bearer " + settings.DevToken}}
	connection, _, err := websocket.Dial(ctx, settings.HubURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return err
	}
	defer connection.CloseNow()
	connection.SetReadLimit(64 << 10)

	hello := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageAgentHello, Hello: &protocol.AgentHello{DeviceID: settings.DeviceID, Name: settings.Name, Platform: runtime.GOOS, Architecture: runtime.GOARCH, AgentVersion: version, Capabilities: []string{"device.info", "tmux.sessions.list", "tmux.sessions.manage", "terminal.stream.placeholder"}}}
	if err := write(ctx, connection, hello); err != nil {
		return err
	}
	logger.Info("agent connected", "hub", settings.HubURL, "deviceId", settings.DeviceID)

	readErrors := make(chan error, 1)
	go func() {
		for {
			messageType, payload, err := connection.Read(ctx)
			if err != nil {
				readErrors <- err
				return
			}
			if messageType != websocket.MessageText {
				continue
			}
			var envelope protocol.Envelope
			if json.Unmarshal(payload, &envelope) != nil || envelope.Type != protocol.MessageRPCRequest {
				continue
			}
			response := handleRPC(ctx, envelope)
			if err := write(ctx, connection, response); err != nil {
				readErrors <- err
				return
			}
		}
	}()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-readErrors:
			return err
		case <-ticker.C:
			if err := write(ctx, connection, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageHeartbeat, Heartbeat: &protocol.Heartbeat{DeviceID: settings.DeviceID, SentAt: time.Now().UTC()}}); err != nil {
				return err
			}
		}
	}
}

func write(ctx context.Context, connection *websocket.Conn, envelope protocol.Envelope) error {
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return connection.Write(writeCtx, websocket.MessageText, payload)
}
