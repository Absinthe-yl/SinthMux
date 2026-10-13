package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// A connector that goes silent (dead path, no FIN or RST) must be dropped and
// shown offline after IdleTimeout, while one that keeps sending heartbeats stays.
func TestSilentConnectorIsDropped(t *testing.T) {
	registry := devices.NewRegistry()
	manager := NewManager()
	server := httptest.NewServer(ConnectorHandler{Registry: registry, Manager: manager, DevToken: "test", IdleTimeout: 300 * time.Millisecond})
	t.Cleanup(server.Close)
	ctx := context.Background()
	dial := func(id string) *websocket.Conn {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer test"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		if err := writeEnvelope(ctx, conn, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: id}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := conn.Read(ctx); err != nil { // ack
			t.Fatal(err)
		}
		return conn
	}
	silent := dial("silent")
	alive := dial("alive")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = writeEnvelope(ctx, alive, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageHeartbeat, Heartbeat: &protocol.Heartbeat{DeviceID: "alive", SentAt: time.Now()}})
			}
		}
	}()
	// The silent client does not read, so it would not notice a close frame;
	// the Hub side is what matters.
	deadline := time.Now().Add(3 * time.Second)
	for manager.Online("silent") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if manager.Online("silent") {
		t.Fatal("silent connector still online after idle timeout")
	}
	status := map[string]string{}
	for _, d := range registry.List() {
		status[d.ID] = d.Status
	}
	if status["silent"] != "offline" {
		t.Fatalf("silent device status %q, want offline", status["silent"])
	}
	if !manager.Online("alive") || status["alive"] != "online" {
		t.Fatalf("heartbeating connector was dropped: online=%v status=%q", manager.Online("alive"), status["alive"])
	}
	_ = silent
}
