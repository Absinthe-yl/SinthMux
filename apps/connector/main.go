package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	if len(os.Args) > 1 && os.Args[1] == "notify" {
		os.Exit(notifyCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	settings := config.ConnectorFromEnv()
	paired := false
	if settings.DeviceToken == "" {
		if saved, err := loadPairedConfig(); err == nil {
			settings, paired = saved, true
		}
	}
	logger := slog.New(slog.NewTextHandler(logOutput(), nil))
	backoff := time.Second
	go cleanUploads(logger)
	var notices *notifier
	if notifySupported {
		notices = newNotifier(logger)
		go notices.watch(context.Background())
	}

	for {
		if paired {
			refreshAndSave(&settings, logger)
		}
		if err := run(context.Background(), &settings, paired, logger, notices); err != nil {
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

// logOutput returns SINTHMUX_CONNECTOR_LOG or the platform default log file,
// falling back to stdout. Logs over 5 MiB are restarted on launch.
func logOutput() io.Writer {
	path := os.Getenv("SINTHMUX_CONNECTOR_LOG")
	if path == "" {
		path = defaultLogPath()
	}
	if path == "" {
		return os.Stdout
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return os.Stdout
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return os.Stdout
	}
	return file
}

// refreshAndSave upgrades or renews the device certificate before connecting.
// Failures keep the current credential so a Hub outage does not drop the device.
func refreshAndSave(settings *config.Connector, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	next := *settings
	changed, err := refreshCredential(ctx, &next)
	if err != nil {
		logger.Warn("device certificate refresh failed", "error", err)
		return
	}
	if !changed {
		return
	}
	path, err := pairedConfigPath()
	if err == nil {
		err = saveConfig(path, next)
	}
	if err != nil {
		logger.Warn("device certificate not saved", "error", err)
		return
	}
	*settings = next
	logger.Info("device certificate updated", "deviceId", settings.DeviceID)
}

// run keeps one connector connection. Paired devices also check hourly whether
// their certificate needs renewal, because a connection can outlive it.
func run(parent context.Context, settings *config.Connector, refresh bool, logger *slog.Logger, notices *notifier) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	headers, err := connectHeaders(ctx, *settings)
	if err != nil {
		return err
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

	hello := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: settings.DeviceID, Name: settings.Name, Platform: runtime.GOOS, Architecture: runtime.GOARCH, ConnectorVersion: version, Capabilities: capabilities()}}
	if err := send(hello); err != nil {
		return err
	}
	logger.Info("connector connected", "hub", settings.HubURL, "deviceId", settings.DeviceID)

	readErrors := make(chan error, 2)
	fail := func(err error) {
		select {
		case readErrors <- err:
		default:
		}
	}
	if notices != nil {
		go notices.publish(ctx, func(snapshot protocol.NotificationSnapshot) error {
			return send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageNotifications, Notifications: &snapshot})
		})
	}
	// RPCs run concurrently so a slow tmux command or a large transfer never
	// blocks terminal input arriving on the same connection.
	rpcSlots := make(chan struct{}, 8)
	go func() {
		for {
			messageType, payload, err := connection.Read(ctx)
			if err != nil {
				fail(err)
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
				select {
				case rpcSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				go func(envelope protocol.Envelope) {
					defer func() { <-rpcSlots }()
					if err := send(handleRPC(ctx, envelope)); err != nil {
						fail(err)
					}
				}(envelope)
			case protocol.MessageStreamOpen:
				streams.open(envelope.StreamOpen)
				// Opening a session in the browser means the user has seen it.
				if notices != nil && envelope.StreamOpen != nil {
					go notices.clear(context.Background(), envelope.StreamOpen.Session)
				}
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
	renewal := time.NewTicker(time.Hour)
	defer renewal.Stop()
	for {
		select {
		case err := <-readErrors:
			return err
		case <-renewal.C:
			if refresh {
				refreshAndSave(settings, logger)
			}
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
