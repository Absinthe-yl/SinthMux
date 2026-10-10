package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	grant    TerminalGrant
}

type TerminalGrant struct {
	UserID      string
	SpaceID     string
	DeviceID    string
	SessionHash []byte
}

type TerminalHandler struct {
	Manager          *Manager
	mu               sync.Mutex
	tickets          map[string]terminalTicket
	ValidateGrant    func(context.Context, TerminalGrant) bool
	OriginPattern    string
	LANOriginPattern string
	// PingInterval is how often the Hub pings the browser; 0 means 30 seconds.
	PingInterval time.Duration
}

func NewTerminalHandler(manager *Manager) *TerminalHandler {
	return &TerminalHandler{Manager: manager, tickets: make(map[string]terminalTicket)}
}

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

func (h *TerminalHandler) Issue(deviceID, session string) (string, error) {
	return h.IssueWithGrant(deviceID, session, TerminalGrant{})
}

func (h *TerminalHandler) IssueWithGrant(deviceID, session string, grant TerminalGrant) (string, error) {
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
	h.tickets[ticketKey(ticket)] = terminalTicket{deviceID: deviceID, session: session, expires: time.Now().Add(45 * time.Second), grant: grant}
	h.mu.Unlock()
	return ticket, nil
}

func (h *TerminalHandler) consume(ticket string) (terminalTicket, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := ticketKey(ticket)
	item, ok := h.tickets[key]
	delete(h.tickets, key)
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
	if h.ValidateGrant != nil {
		cookie, err := r.Cookie("sinthmux_session")
		if err != nil {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		hash := sha256.Sum256([]byte(cookie.Value))
		if subtle.ConstantTimeCompare(hash[:], item.grant.SessionHash) != 1 || !h.ValidateGrant(r.Context(), item.grant) {
			http.Error(w, "ticket permission denied", http.StatusForbidden)
			return
		}
	}
	origins := []string{"localhost:*", "127.0.0.1:*"}
	if h.OriginPattern != "" {
		origins = []string{h.OriginPattern}
	}
	if h.LANOriginPattern != "" {
		origins = append(origins, h.LANOriginPattern)
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: origins, Subprotocols: []string{"sinthmux.v1"}})
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
	validationTicker := time.NewTicker(2 * time.Second)
	defer validationTicker.Stop()
	// Ping the browser so a vanished client (closed laptop, dropped mobile
	// network) releases its tmux attach instead of lingering until TCP gives up.
	pingInterval := h.PingInterval
	if pingInterval <= 0 {
		pingInterval = 30 * time.Second
	}
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()
	pingFailed := make(chan struct{}, 1)
	pinging := false
	pingDone := make(chan struct{}, 1)
	readErrors := make(chan error, 1)
	go func() {
		for {
			typeOfMessage, body, readErr := conn.Read(ctx)
			if readErr != nil {
				readErrors <- readErr
				return
			}
			// Permission is rechecked by validationTicker, not per message, so
			// typing or pasting does not run a database query per frame.
			var envelope protocol.Envelope
			switch typeOfMessage {
			case websocket.MessageBinary:
				envelope = protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: id, Data: body}}
			case websocket.MessageText:
				var control struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if json.Unmarshal(body, &control) != nil {
					continue
				}
				// The browser pings to detect a silently dead connection; the Hub
				// answers itself so a busy device cannot delay the reply.
				if control.Type == "ping" {
					if len(control.ID) > 64 {
						continue
					}
					pong, _ := json.Marshal(map[string]string{"type": "pong", "id": control.ID})
					if writeErr := conn.Write(ctx, websocket.MessageText, pong); writeErr != nil {
						readErrors <- writeErr
						return
					}
					continue
				}
				if control.Type != "resize" || control.Cols == 0 || control.Rows == 0 {
					continue
				}
				envelope = protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamResize, StreamResize: &protocol.StreamResize{StreamID: id, Cols: control.Cols, Rows: control.Rows}}
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
		case <-pingTicker.C:
			if pinging {
				continue
			}
			pinging = true
			go func() {
				pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if conn.Ping(pingCtx) != nil {
					pingFailed <- struct{}{}
				}
				pingDone <- struct{}{}
			}()
		case <-pingDone:
			pinging = false
		case <-pingFailed:
			// A peer that ignores pings will not answer a close handshake either.
			_ = conn.CloseNow()
			return
		case <-validationTicker.C:
			if h.ValidateGrant != nil && !h.ValidateGrant(ctx, item.grant) {
				_ = conn.Close(websocket.StatusPolicyViolation, "permission revoked")
				return
			}
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
