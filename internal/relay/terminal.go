package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
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
	// WriteTimeout 限制单次浏览器写入（含等待写锁），默认 5 秒。
	WriteTimeout time.Duration

	validationInterval time.Duration
	validationTimeout  time.Duration
}

// 保留底层连接，确保 Close 已取得 closing 标记时仍能硬关闭传输。
// coder/websocket 的 CloseNow 在这种情况下只会等待 Close 完成。
type terminalUpgradeWriter struct {
	http.ResponseWriter
	transport net.Conn
}

func (w *terminalUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	w.transport = conn
	return conn, rw, err
}

type terminalSocket struct {
	*websocket.Conn
	transport net.Conn
}

func (c terminalSocket) CloseNow() error {
	_ = c.transport.Close()
	return c.Conn.CloseNow()
}

func (h *TerminalHandler) writeTimeout() time.Duration {
	if h.WriteTimeout > 0 {
		return h.WriteTimeout
	}
	return 5 * time.Second
}

func (h *TerminalHandler) writeBrowser(ctx context.Context, conn interface {
	Write(context.Context, websocket.MessageType, []byte) error
}, kind websocket.MessageType, body []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, h.writeTimeout())
	defer cancel()
	return conn.Write(writeCtx, kind, body)
}

func (h *TerminalHandler) validateGrant(ctx context.Context, grant TerminalGrant) bool {
	timeout := h.validationTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// ValidateGrant 必须将 context 传给数据库；超时后即使返回 true 也拒绝。
	valid := h.ValidateGrant(checkCtx, grant)
	return valid && checkCtx.Err() == nil
}

func closeBrowser(conn terminalSocket, code websocket.StatusCode, reason string, timeout time.Duration) {
	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		defer close(done)
		_ = conn.CloseNow()
	})
	_ = conn.Close(code, reason)
	if !timer.Stop() {
		<-done
	}
}

func (h *TerminalHandler) watchGrant(ctx context.Context, cancel context.CancelFunc, conn terminalSocket, grant TerminalGrant) {
	if h.ValidateGrant == nil {
		return
	}
	interval := h.validationInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			valid := h.validateGrant(ctx, grant)
			if ctx.Err() != nil {
				return
			}
			if !valid {
				// 先取消流及在途写入，不等待主循环，也不依赖 1008 送达。
				cancel()
				closeBrowser(conn, websocket.StatusPolicyViolation, "permission revoked", min(h.writeTimeout(), 100*time.Millisecond))
				return
			}
		}
	}
}

func (h *TerminalHandler) pingBrowser(ctx context.Context, cancel context.CancelFunc, conn terminalSocket) {
	interval := h.PingInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
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
		if subtle.ConstantTimeCompare(hash[:], item.grant.SessionHash) != 1 || !h.validateGrant(r.Context(), item.grant) {
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
	upgrade := &terminalUpgradeWriter{ResponseWriter: w}
	ws, err := websocket.Accept(upgrade, r, &websocket.AcceptOptions{OriginPatterns: origins, Subprotocols: []string{"sinthmux.v1"}})
	if err != nil {
		return
	}
	conn := terminalSocket{Conn: ws, transport: upgrade.transport}
	conn.SetReadLimit(16 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	// 撤权后只暂留浏览器读取以完成 1008 握手；流 context 和应用写立即取消。
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	validationDone := make(chan struct{})
	go func() {
		defer close(validationDone)
		h.watchGrant(ctx, cancel, conn, item.grant)
	}()
	var workers sync.WaitGroup
	var stop func()
	defer func() {
		cancel()
		<-validationDone
		cancelRead()
		_ = conn.CloseNow()
		if stop != nil {
			stop()
		}
		workers.Wait()
	}()
	// 复核在 OpenStream 之前启动，打开流期间也必须及时关闭浏览器。
	id, events, stop, err := h.Manager.OpenStream(ctx, item.deviceID, item.session, 80, 24)
	if err != nil {
		if ctx.Err() == nil {
			closeBrowser(conn, websocket.StatusInternalError, err.Error(), h.writeTimeout())
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		h.pingBrowser(ctx, cancel, conn)
	}()
	readErrors := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			typeOfMessage, body, readErr := conn.Read(readCtx)
			if readErr != nil {
				readErrors <- readErr
				return
			}
			// 独立生命周期定期复核权限，输入和粘贴不逐帧查询数据库。
			if ctx.Err() != nil {
				return
			}
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
					if writeErr := h.writeBrowser(ctx, conn, websocket.MessageText, pong); writeErr != nil {
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
		case <-readErrors:
			return
		case envelope, ok := <-events:
			if !ok || ctx.Err() != nil {
				return
			}
			switch envelope.Type {
			case protocol.MessageStreamData:
				if envelope.StreamData == nil {
					continue
				}
				if err := h.writeBrowser(ctx, conn, websocket.MessageBinary, envelope.StreamData.Data); err != nil {
					return
				}
			case protocol.MessageStreamClose:
				reason := "终端连接已结束"
				if envelope.StreamClose != nil && envelope.StreamClose.Reason != "" {
					reason = envelope.StreamClose.Reason
				}
				closeBrowser(conn, websocket.StatusNormalClosure, reason, h.writeTimeout())
				return
			}
		}
	}
}
