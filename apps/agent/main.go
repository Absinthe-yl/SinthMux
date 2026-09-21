package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

	hello := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageAgentHello, Hello: &protocol.AgentHello{DeviceID: settings.DeviceID, Name: settings.Name, Platform: runtime.GOOS, Architecture: runtime.GOARCH, AgentVersion: version, Capabilities: []string{"device.info", "tmux.sessions.list", "terminal.stream.placeholder"}}}
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

func handleRPC(ctx context.Context, envelope protocol.Envelope) protocol.Envelope {
	result := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: envelope.RequestID, Response: &protocol.RPCResponse{}}
	if envelope.Version != protocol.Version || envelope.RequestID == "" || envelope.Request == nil {
		result.Response.Error = "invalid RPC request"
		return result
	}
	if envelope.Request.Method != "tmux.sessions.list" {
		result.Response.Error = "unsupported RPC method"
		return result
	}
	sessions, err := listSessions(ctx)
	if err != nil {
		result.Response.Error = err.Error()
		return result
	}
	result.Response.OK = true
	result.Response.Sessions = sessions
	return result
}

func listSessions(ctx context.Context) ([]protocol.TmuxSession, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "tmux", "list-sessions", "-F", "#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}").CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "no server running") || strings.Contains(string(output), "failed to connect to server") {
			return []protocol.TmuxSession{}, nil
		}
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, fmt.Errorf("tmux list-sessions: %w: %s", err, strings.TrimSpace(string(output)))
	}
	sessions := make([]protocol.TmuxSession, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("invalid tmux session output")
		}
		windows, windowsErr := strconv.Atoi(fields[1])
		attached, attachedErr := strconv.Atoi(fields[2])
		createdAt, createdErr := strconv.ParseInt(fields[3], 10, 64)
		if windowsErr != nil || attachedErr != nil || createdErr != nil {
			return nil, fmt.Errorf("invalid tmux session output")
		}
		sessions = append(sessions, protocol.TmuxSession{Name: fields[0], Windows: windows, Attached: attached > 0, CreatedAt: createdAt})
	}
	return sessions, nil
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
