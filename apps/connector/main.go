package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// A dead path to the Hub is noticed within hubPingInterval + hubPongTimeout.
var (
	hubPingInterval = 15 * time.Second
	hubPongTimeout  = 10 * time.Second
)

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
		started := time.Now()
		err := run(context.Background(), &settings, paired, logger, notices)
		// run always returns an error; a connection that stayed up for a while
		// starts the backoff again from one second.
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		// Pooled keep-alive connections to the Hub may have died with the
		// WebSocket (network switch); reusing one would hang the nonce request.
		httpClient.CloseIdleConnections()
		logger.Warn("connector disconnected", "error", err, "retryIn", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, 15*time.Second)
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
	return runWithRPC(parent, settings, refresh, logger, notices, processRPCSlots, handleRPC)
}

func runWithRPC(parent context.Context, settings *config.Connector, refresh bool, logger *slog.Logger, notices *notifier, slots chan struct{}, execute func(context.Context, protocol.Envelope) protocol.Envelope) error {
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
		cancel()
	}
	if notices != nil {
		go notices.publish(ctx, func(snapshot protocol.NotificationSnapshot) error {
			return send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageNotifications, Notifications: &snapshot})
		})
	}
	rpcs := newRPCDispatcher(ctx, slots, send, execute, fail)
	var connectionWorkers sync.WaitGroup
	defer func() {
		cancel()
		_ = connection.CloseNow()
		streams.closeAll()
		// 仅回收连接级协程。RPC 可能卡在不可取消的 OS I/O，不能阻止重连；
		// 它们继续占用进程共享槽位，直到执行及旧连接的回包处理结束。
		connectionWorkers.Wait()
		<-rpcs.writerDone
	}()
	connectionWorkers.Go(func() {
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
				if err := rpcs.dispatch(envelope); err != nil {
					fail(err)
					return
				}
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
	})

	// Writes into a dead TCP path still succeed (they only fill the socket
	// buffer), so the heartbeat alone cannot tell that the Hub is gone. A
	// WebSocket ping must be answered; without a pong the connection is
	// dropped and the outer loop reconnects.
	connectionWorkers.Go(func() {
		pings := time.NewTicker(hubPingInterval)
		defer pings.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pings.C:
			}
			pingCtx, cancelPing := context.WithTimeout(ctx, hubPongTimeout)
			err := connection.Ping(pingCtx)
			cancelPing()
			if err != nil && ctx.Err() == nil {
				fail(fmt.Errorf("Hub 无响应: %w", err))
				return
			}
		}
	})

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	renewal := time.NewTicker(time.Hour)
	defer renewal.Stop()
	for {
		select {
		case <-ctx.Done():
			select {
			case err := <-readErrors:
				return err
			default:
				return ctx.Err()
			}
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

const (
	rpcConcurrency       = 8
	rpcOverloadQueueSize = 8
)

// 跨 run 共享，旧连接残留的不可取消 I/O 也计入 8 个在途 RPC 的上限。
var processRPCSlots = make(chan struct{}, rpcConcurrency)

type rpcDispatcher struct {
	ctx        context.Context
	slots      chan struct{}
	overload   chan protocol.Envelope
	send       func(protocol.Envelope) error
	execute    func(context.Context, protocol.Envelope) protocol.Envelope
	fail       func(error)
	workers    sync.WaitGroup
	writerDone chan struct{}
}

func newRPCDispatcher(ctx context.Context, slots chan struct{}, send func(protocol.Envelope) error, execute func(context.Context, protocol.Envelope) protocol.Envelope, fail func(error)) *rpcDispatcher {
	d := &rpcDispatcher{
		ctx: ctx, slots: slots,
		overload: make(chan protocol.Envelope, rpcOverloadQueueSize),
		send:     send, execute: execute, fail: fail,
		writerDone: make(chan struct{}),
	}
	// 仅这个 writer 发送过载响应；读循环不等待 writeMu 或网络写入。
	go func() {
		defer close(d.writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case response := <-d.overload:
				if ctx.Err() != nil {
					return
				}
				if err := send(response); err != nil {
					fail(err)
					return
				}
			}
		}
	}()
	return d
}

func (d *rpcDispatcher) dispatch(envelope protocol.Envelope) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	select {
	case d.slots <- struct{}{}:
		d.workers.Go(func() {
			// 正常回包发送完成前不释放槽位，最多保留 8 个在途响应。
			defer func() { <-d.slots }()
			if d.ctx.Err() != nil {
				return
			}
			response := d.execute(d.ctx, envelope)
			// 忽略取消的执行器返回时，丢弃旧代结果；send 也只绑定本代 ctx/连接。
			if d.ctx.Err() != nil {
				return
			}
			if err := d.send(response); err != nil {
				d.fail(err)
			}
		})
		return nil
	default:
	}
	response := protocol.Envelope{
		Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: envelope.RequestID,
		Response: &protocol.RPCResponse{OK: false, ErrorCode: "busy", Error: "connector RPC concurrency limit reached"},
	}
	select {
	case <-d.ctx.Done():
		return d.ctx.Err()
	case d.overload <- response:
		return nil
	default:
		// 无法有界地回包时让 run 关闭连接，不静默丢弃或增建 goroutine。
		return fmt.Errorf("RPC 过载响应队列已满")
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
