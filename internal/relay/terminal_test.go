package relay

import (
	"errors"
	"testing"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestTerminalTicketSingleUseAndExpiry(t *testing.T) {
	manager := NewManager()
	handler := NewTerminalHandler(manager)
	if _, err := handler.Issue("device", "work"); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline device: got %v", err)
	}
	manager.Register("device", nil)
	if _, err := handler.Issue("device", "bad/name"); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("invalid session name: got %v", err)
	}
	ticket, err := handler.Issue("device", "work")
	if err != nil {
		t.Fatal(err)
	}
	item, ok := handler.consume(ticket)
	if !ok || item.deviceID != "device" || item.session != "work" {
		t.Fatalf("unexpected ticket: %+v, %t", item, ok)
	}
	if _, ok := handler.consume(ticket); ok {
		t.Fatal("ticket replay accepted")
	}
	handler.mu.Lock()
	handler.tickets[ticket] = terminalTicket{deviceID: "device", session: "work", expires: time.Now().Add(-time.Second)}
	handler.mu.Unlock()
	if _, ok := handler.consume(ticket); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestStreamEventsAndDisconnect(t *testing.T) {
	agent := &agentConnection{streams: make(map[string]chan protocol.Envelope), pending: make(map[string]chan rpcResult)}
	first := make(chan protocol.Envelope, 2)
	agent.streams["first"] = first
	agent.streamEvent(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: "first", Data: []byte("hello")}})
	if got := <-first; string(got.StreamData.Data) != "hello" {
		t.Fatalf("unexpected output: %+v", got)
	}
	agent.streamEvent(protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: "first", Reason: "done"}})
	if got := <-first; got.StreamClose == nil || got.StreamClose.Reason != "done" {
		t.Fatalf("unexpected close: %+v", got)
	}
	if len(agent.streams) != 0 {
		t.Fatal("closed stream still registered")
	}
	second := make(chan protocol.Envelope, 1)
	agent.streams["second"] = second
	agent.failAll(ErrOffline)
	if got := <-second; got.StreamClose == nil || got.StreamClose.Reason != ErrOffline.Error() {
		t.Fatalf("unexpected disconnect: %+v", got)
	}
}
