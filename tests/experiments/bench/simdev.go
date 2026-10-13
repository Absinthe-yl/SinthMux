package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// simDevice is a virtual connector for load tests. It speaks the connector
// protocol, answers session RPCs, sends a 2 KiB "screen" on stream.open and
// echoes terminal input back, so the Hub does exactly the relay work it does
// for a real device without needing a pty per terminal on the test machine
// (macOS allows 511 ptys in total).
type simDevice struct {
	ID, Token, Name string
	hubWS           string
	conn            *websocket.Conn
	mu              sync.Mutex // serialises writes
	streams         sync.Map   // streamID → chan struct{} (closed on stream.close)
	streamRate      int        // bytes per second of output per stream; 0 = none
	cancel          context.CancelFunc
	done            chan struct{}
	opened          atomic.Int64
}

var screen = bytes.Repeat([]byte("."), 2048)

// createSimDevices registers n devices in the space with the device-token API
// (POST /spaces/{id}/devices), which is not rate limited like pairing codes.
func createSimDevices(owner *client, spaceID, prefix string, n, parallel int) ([]*simDevice, error) {
	devices := make([]*simDevice, n)
	errs := make(chan error, n)
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			var out struct {
				Device struct{ ID string }
				Token  string
			}
			name := fmt.Sprintf("%s-%04d", prefix, i)
			status, err := owner.call(http.MethodPost, "/api/v1/spaces/"+spaceID+"/devices", map[string]string{"name": name}, &out)
			if err != nil || status != http.StatusCreated {
				errs <- fmt.Errorf("create device %s: HTTP %d %v", name, status, err)
				return
			}
			devices[i] = &simDevice{ID: out.Device.ID, Token: out.Token, Name: name}
		}(i)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return devices, nil
}

func (d *simDevice) connect(hub string) error {
	d.hubWS = strings.Replace(strings.TrimRight(hub, "/"), "http", "ws", 1) + "/ws/v1/connectors/connect"
	header := http.Header{"Authorization": {"Bearer " + d.Token}, "X-Sinthmux-Device-Id": {d.ID}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, _, err := websocket.Dial(ctx, d.hubWS, &websocket.DialOptions{HTTPHeader: header, HTTPClient: &http.Client{Transport: &http.Transport{Proxy: nil}}})
	cancel()
	if err != nil {
		return err
	}
	conn.SetReadLimit(64 << 10)
	d.conn = conn
	runCtx, stop := context.WithCancel(context.Background())
	d.cancel, d.done = stop, make(chan struct{})
	if err := d.send(protocol.Envelope{Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: d.ID, Name: d.Name, Platform: "sim", Architecture: "sim", ConnectorVersion: "bench", Capabilities: []string{}}}); err != nil {
		return err
	}
	go d.readLoop(runCtx)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = d.send(protocol.Envelope{Type: protocol.MessageHeartbeat, Heartbeat: &protocol.Heartbeat{DeviceID: d.ID, SentAt: time.Now().UTC()}})
			}
		}
	}()
	return nil
}

func (d *simDevice) send(envelope protocol.Envelope) error {
	envelope.Version = protocol.Version
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.conn.Write(ctx, websocket.MessageText, payload)
}

func (d *simDevice) readLoop(ctx context.Context) {
	defer close(d.done)
	for {
		_, payload, err := d.conn.Read(ctx)
		if err != nil {
			return
		}
		var envelope protocol.Envelope
		if json.Unmarshal(payload, &envelope) != nil {
			continue
		}
		switch envelope.Type {
		case protocol.MessageRPCRequest:
			response := &protocol.RPCResponse{OK: true}
			if envelope.Request != nil {
				switch envelope.Request.Method {
				case "tmux.sessions.list":
					response.Sessions = []protocol.TmuxSession{{Name: "s", Windows: 1}}
				case "tmux.sessions.create", "tmux.sessions.close", "tmux.sessions.rename", "session.notify.clear":
				default:
					response = &protocol.RPCResponse{OK: false, Error: "unsupported", ErrorCode: "unsupported"}
				}
			}
			go func(id string) {
				_ = d.send(protocol.Envelope{Type: protocol.MessageRPCResponse, RequestID: id, Response: response})
			}(envelope.RequestID)
		case protocol.MessageStreamOpen:
			if envelope.StreamOpen == nil {
				continue
			}
			id := envelope.StreamOpen.StreamID
			stop := make(chan struct{})
			d.streams.Store(id, stop)
			d.opened.Add(1)
			go func() {
				_ = d.send(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: id, Data: screen}})
				if d.streamRate > 0 {
					d.stream(id, stop)
				}
			}()
		case protocol.MessageStreamData:
			if envelope.StreamData != nil {
				data := envelope.StreamData
				_ = d.send(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: data.StreamID, Data: data.Data}})
			}
		case protocol.MessageStreamClose:
			if envelope.StreamClose != nil {
				if stop, ok := d.streams.LoadAndDelete(envelope.StreamClose.StreamID); ok {
					close(stop.(chan struct{}))
				}
			}
		}
	}
}

// stream sends output at streamRate in 4 KiB chunks, like a build log.
func (d *simDevice) stream(id string, stop <-chan struct{}) {
	chunk := bytes.Repeat([]byte("-"), 4096)
	interval := time.Duration(float64(time.Second) * 4096 / float64(d.streamRate))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-d.done:
			return
		case <-ticker.C:
			if d.send(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: id, Data: chunk}}) != nil {
				return
			}
		}
	}
}

func (d *simDevice) close() {
	if d.cancel != nil {
		d.cancel()
	}
	if d.conn != nil {
		_ = d.conn.CloseNow()
	}
}

// connectAll connects devices with bounded parallelism and waits until the Hub lists them online.
func connectAll(devices []*simDevice, hub string, parallel int) error {
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var failed atomic.Int64
	var firstErr atomic.Value
	for _, d := range devices {
		wg.Add(1)
		slots <- struct{}{}
		go func(d *simDevice) {
			defer wg.Done()
			defer func() { <-slots }()
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				if err = d.connect(hub); err == nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			failed.Add(1)
			firstErr.CompareAndSwap(nil, err)
		}(d)
	}
	wg.Wait()
	if failed.Load() > 0 {
		return fmt.Errorf("%d devices failed to connect: %v", failed.Load(), firstErr.Load())
	}
	return nil
}

func waitOnline(owner *client, ids []string, timeout time.Duration) error {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	deadline := time.Now().Add(timeout)
	for {
		list, err := owner.devices()
		if err == nil {
			online := 0
			for _, d := range list {
				if want[d.ID] && d.Status == "online" {
					online++
				}
			}
			if online == len(want) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("only %d/%d devices online", online, len(want))
			}
		}
		if time.Now().After(deadline) {
			return errors.New("device list unavailable")
		}
		time.Sleep(300 * time.Millisecond)
	}
}
