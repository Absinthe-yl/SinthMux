package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
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
	handler.tickets[ticketKey(ticket)] = terminalTicket{deviceID: "device", session: "work", expires: time.Now().Add(-time.Second)}
	handler.mu.Unlock()
	if _, ok := handler.consume(ticket); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestTerminalTicketBindsBrowserSession(t *testing.T) {
	manager := NewManager()
	manager.Register("device", nil)
	handler := NewTerminalHandler(manager)
	hash := sha256.Sum256([]byte("session-secret"))
	handler.ValidateGrant = func(context.Context, TerminalGrant) bool { return false }
	request := func(cookie string) int {
		ticket, err := handler.IssueWithGrant("device", "work", TerminalGrant{SessionHash: hash[:]})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "http://127.0.0.1:8090/ws/v1/terminal", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "sinthmux.v1, sinthmux.ticket."+ticket)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "sinthmux_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request(""); got != 401 {
		t.Fatalf("missing session status=%d", got)
	}
	if got := request("another-session"); got != 403 {
		t.Fatalf("wrong session status=%d", got)
	}
	if got := request("session-secret"); got != 403 {
		t.Fatalf("revoked grant status=%d", got)
	}
}

func TestStreamEventsAndDisconnect(t *testing.T) {
	connector := &connectorConnection{streams: make(map[string]chan protocol.Envelope), pending: make(map[string]chan rpcResult)}
	first := make(chan protocol.Envelope, 2)
	connector.streams["first"] = first
	connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: "first", Data: []byte("hello")}})
	if got := <-first; string(got.StreamData.Data) != "hello" {
		t.Fatalf("unexpected output: %+v", got)
	}
	connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: "first", Reason: "done"}})
	if got := <-first; got.StreamClose == nil || got.StreamClose.Reason != "done" {
		t.Fatalf("unexpected close: %+v", got)
	}
	if len(connector.streams) != 0 {
		t.Fatal("closed stream still registered")
	}
	second := make(chan protocol.Envelope, 1)
	connector.streams["second"] = second
	connector.failAll(ErrOffline)
	if got := <-second; got.StreamClose == nil || got.StreamClose.Reason != ErrOffline.Error() {
		t.Fatalf("unexpected disconnect: %+v", got)
	}
}
