package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

const version = "0.0.1-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "pair" {
		if err := pair(os.Args[2:]); err != nil {
			_, _ = os.Stderr.WriteString("设备配对失败：" + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}
	settings := config.ConnectorFromEnv()
	if settings.DeviceToken == "" {
		if saved, err := loadPairedConfig(); err == nil {
			settings = saved
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	backoff := time.Second

	for {
		if err := run(context.Background(), settings, logger); err != nil {
			logger.Warn("connector disconnected", "error", err, "retryIn", backoff)
			time.Sleep(backoff)
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func run(ctx context.Context, settings config.Connector, logger *slog.Logger) error {
	headers := http.Header{"Authorization": []string{"Bearer " + settings.DevToken}}
	if settings.DeviceToken != "" {
		headers.Set("Authorization", "Bearer "+settings.DeviceToken)
		headers.Set("X-Sinthmux-Device-ID", settings.DeviceID)
	}
	connection, _, err := websocket.Dial(ctx, settings.HubURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		return err
	}
	defer connection.CloseNow()
	connection.SetReadLimit(64 << 10)
	var writeMu sync.Mutex
	send := func(envelope protocol.Envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return write(ctx, connection, envelope)
	}
	streams := newTerminalStreams(send)
	defer streams.closeAll()

	hello := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: settings.DeviceID, Name: settings.Name, Platform: runtime.GOOS, Architecture: runtime.GOARCH, ConnectorVersion: version, Capabilities: []string{"device.info", "tmux.sessions.list", "tmux.sessions.manage", "terminal.stream"}}}
	if err := send(hello); err != nil {
		return err
	}
	logger.Info("connector connected", "hub", settings.HubURL, "deviceId", settings.DeviceID)

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
			if json.Unmarshal(payload, &envelope) != nil || envelope.Version != protocol.Version {
				continue
			}
			switch envelope.Type {
			case protocol.MessageRPCRequest:
				response := handleRPC(ctx, envelope)
				if err := send(response); err != nil {
					readErrors <- err
					return
				}
			case protocol.MessageStreamOpen:
				streams.open(envelope.StreamOpen)
			case protocol.MessageStreamData:
				streams.input(envelope.StreamData)
			case protocol.MessageStreamResize:
				streams.resize(envelope.StreamResize)
			case protocol.MessageStreamClose:
				if envelope.StreamClose != nil {
					streams.close(envelope.StreamClose.StreamID)
				}
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
			if err := send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageHeartbeat, Heartbeat: &protocol.Heartbeat{DeviceID: settings.DeviceID, SentAt: time.Now().UTC()}}); err != nil {
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
