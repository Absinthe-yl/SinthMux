package relay

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// terminalHarness runs a TerminalHandler with a fake connector that answers
// stream.open with one burst of output and records what else it receives.
func terminalHarness(t *testing.T, ping time.Duration) (*httptest.Server, *TerminalHandler, chan protocol.Envelope) {
	t.Helper()
	manager := NewManager()
	handler := NewTerminalHandler(manager)
	handler.PingInterval = ping
	received := make(chan protocol.Envelope, 32)
	router := http.NewServeMux()
	router.Handle("/ws/v1/terminal", handler)
	registered := make(chan *connectorConnection, 1)
	router.HandleFunc("/connector", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		registered <- manager.Register("device", conn)
		<-r.Context().Done()
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	device, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/connector", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { device.CloseNow() })
	connector := <-registered
	// The fake device reads what the Hub sends it and answers stream.open
	// with one screen of output, as tmux attach would.
	go func() {
		for {
			_, payload, err := device.Read(ctx)
			if err != nil {
				return
			}
			var envelope protocol.Envelope
			_ = json.Unmarshal(payload, &envelope)
			if envelope.Type == protocol.MessageStreamOpen {
				connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: envelope.StreamOpen.StreamID, Data: []byte("screen")}})
			}
			received <- envelope
		}
	}()
	return server, handler, received
}

func openTerminal(t *testing.T, server *httptest.Server, handler *TerminalHandler) *websocket.Conn {
	t.Helper()
	ticket, err := handler.Issue("device", "work")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/v1/terminal", &websocket.DialOptions{Subprotocols: []string{"sinthmux.v1", "sinthmux.ticket." + ticket}, HTTPHeader: http.Header{"Origin": {"http://localhost:5173"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestTerminalAnswersBrowserPing(t *testing.T) {
	server, handler, received := terminalHarness(t, 0)
	conn := openTerminal(t, server, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if kind, data, err := conn.Read(ctx); err != nil || kind != websocket.MessageBinary || string(data) != "screen" {
		t.Fatalf("first frame: %v %q %v", kind, data, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","id":"17"}`)); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText || string(data) != `{"id":"17","type":"pong"}` {
		t.Fatalf("pong: %v %s %v", kind, data, err)
	}
	// The ping never reaches the device; only stream.open did.
	if envelope := <-received; envelope.Type != protocol.MessageStreamOpen {
		t.Fatalf("device received %s", envelope.Type)
	}
	select {
	case envelope := <-received:
		t.Fatalf("ping leaked to the device as %s", envelope.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTerminalClosesSilentBrowser(t *testing.T) {
	server, handler, received := terminalHarness(t, 100*time.Millisecond)
	ticket, err := handler.Issue("device", "work")
	if err != nil {
		t.Fatal(err)
	}
	// A raw client completes the handshake and then never reads, so it
	// cannot answer the Hub's ping, like a frozen mobile network.
	raw, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, _ = raw.Write([]byte("GET /ws/v1/terminal HTTP/1.1\r\nHost: localhost\r\nOrigin: http://localhost:5173\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: sinthmux.v1, sinthmux.ticket." + ticket + "\r\n\r\n"))
	deadline := time.After(15 * time.Second)
	for {
		select {
		case envelope := <-received:
			if envelope.Type == protocol.MessageStreamClose {
				return // the Hub gave up on the browser and released the tmux attach
			}
		case <-deadline:
			t.Fatal("silent browser was never disconnected")
		}
	}
}
