package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type terminalTicket struct {
	deviceID string
	session  string
	expires  time.Time
}

type TerminalHandler struct {
	Manager *Manager
	mu      sync.Mutex
	tickets map[string]terminalTicket
}

func NewTerminalHandler(manager *Manager) *TerminalHandler {
	return &TerminalHandler{Manager: manager, tickets: make(map[string]terminalTicket)}
}

func (h *TerminalHandler) Issue(deviceID, session string) (string, error) {
	if !protocol.ValidSessionName(session) {
		return "", ErrInvalidResponse
	}
	if !h.Manager.Online(deviceID) {
		return "", ErrOffline
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	ticket := hex.EncodeToString(bytes)
	h.mu.Lock()
	for key, item := range h.tickets {
		if time.Now().After(item.expires) {
			delete(h.tickets, key)
		}
	}
	h.tickets[ticket] = terminalTicket{deviceID: deviceID, session: session, expires: time.Now().Add(45 * time.Second)}
	h.mu.Unlock()
	return ticket, nil
}

func (h *TerminalHandler) consume(ticket string) (terminalTicket, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	item, ok := h.tickets[ticket]
	delete(h.tickets, ticket)
	return item, ok && time.Now().Before(item.expires)
}

func (h *TerminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ticket string
	var version bool
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, candidate := range strings.Split(header, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "sinthmux.v1" {
				version = true
			}
			if strings.HasPrefix(candidate, "sinthmux.ticket.") {
				ticket = strings.TrimPrefix(candidate, "sinthmux.ticket.")
			}
		}
	}
	if !version || ticket == "" {
		http.Error(w, "terminal ticket required", http.StatusUnauthorized)
		return
	}
	item, ok := h.consume(ticket)
	if !ok {
		http.Error(w, "invalid or expired terminal ticket", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"localhost:*", "127.0.0.1:*"}, Subprotocols: []string{"sinthmux.v1"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	id, events, stop, err := h.Manager.OpenStream(ctx, item.deviceID, item.session, 80, 24)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer stop()
	readErrors := make(chan error, 1)
	go func() {
		for {
			typeOfMessage, body, readErr := conn.Read(ctx)
			if readErr != nil {
				readErrors <- readErr
				return
			}
			var envelope protocol.Envelope
			switch typeOfMessage {
			case websocket.MessageBinary:
				envelope = protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: id, Data: body}}
			case websocket.MessageText:
				var resize struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if json.Unmarshal(body, &resize) != nil || resize.Type != "resize" || resize.Cols == 0 || resize.Rows == 0 {
					continue
				}
				envelope = protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamResize, StreamResize: &protocol.StreamResize{StreamID: id, Cols: resize.Cols, Rows: resize.Rows}}
			default:
				continue
			}
			if sendErr := h.Manager.SendStream(ctx, item.deviceID, id, envelope); sendErr != nil {
				readErrors <- sendErr
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readErrors:
			return
		case envelope := <-events:
			switch envelope.Type {
			case protocol.MessageStreamData:
				if envelope.StreamData == nil {
					continue
				}
				if err := conn.Write(ctx, websocket.MessageBinary, envelope.StreamData.Data); err != nil {
					return
				}
			case protocol.MessageStreamClose:
				reason := "终端连接已结束"
				if envelope.StreamClose != nil && envelope.StreamClose.Reason != "" {
					reason = envelope.StreamClose.Reason
				}
				_ = conn.Close(websocket.StatusNormalClosure, reason)
				return
			}
		}
	}
}
