package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// net.Pipe 没有内核发送缓冲区；客户端不读时，第一帧就会产生真实背压。
// 配合有限的 stream 队列，无需分配巨量输出或猜测操作系统缓冲区大小。
type terminalObservedConn struct {
	net.Conn
	observe atomic.Bool
	writes  chan struct{}
}

func (c *terminalObservedConn) Write(p []byte) (int, error) {
	if c.observe.Load() {
		select {
		case c.writes <- struct{}{}:
		default:
		}
	}
	return c.Conn.Write(p)
}

type terminalPipeListener struct {
	conn   net.Conn
	accept chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func (l *terminalPipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accept:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *terminalPipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *terminalPipeListener) Addr() net.Addr { return l.conn.LocalAddr() }

func terminalPipePeer(t *testing.T, handler http.Handler, opts *websocket.DialOptions) (*websocket.Conn, *terminalObservedConn) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	observed := &terminalObservedConn{Conn: serverConn, writes: make(chan struct{}, 1)}
	listener := &terminalPipeListener{conn: observed, accept: make(chan net.Conn, 1), done: make(chan struct{})}
	listener.accept <- observed
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = serverConn.Close(); _ = clientConn.Close() })
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	opts.HTTPClient = &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peer, _, err := websocket.Dial(ctx, server.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.CloseNow() })
	observed.observe.Store(true)
	return peer, observed
}

type terminalBackpressureHarness struct {
	browser       *websocket.Conn
	device        *websocket.Conn
	connector     *connectorConnection
	browserWrites <-chan struct{}
	deviceWrites  <-chan struct{}
	finished      <-chan struct{}
}

func newTerminalBackpressureHarness(t *testing.T, configure func(*TerminalHandler), opts *websocket.DialOptions) *terminalBackpressureHarness {
	t.Helper()
	manager := NewManager()
	registered := make(chan *connectorConnection, 1)
	device, deviceTransport := terminalPipePeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		connector := manager.Register("device", conn)
		defer manager.Unregister("device", connector)
		registered <- connector
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}), nil)
	connector := <-registered
	handler := NewTerminalHandler(manager)
	handler.WriteTimeout = 250 * time.Millisecond
	handler.validationInterval = 20 * time.Millisecond
	if configure != nil {
		configure(handler)
	}
	hash := sha256.Sum256([]byte("session-secret"))
	ticket, err := handler.IssueWithGrant("device", "work", TerminalGrant{SessionHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	opts.Subprotocols = []string{"sinthmux.v1", "sinthmux.ticket." + ticket}
	opts.HTTPHeader = http.Header{"Origin": {"http://localhost:5173"}, "Cookie": {"sinthmux_session=session-secret"}}
	finished := make(chan struct{})
	browser, browserTransport := terminalPipePeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}), opts)
	return &terminalBackpressureHarness{
		browser: browser, device: device, connector: connector,
		browserWrites: browserTransport.writes, deviceWrites: deviceTransport.writes, finished: finished,
	}
}

func terminalWait(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待%s超时", what)
	}
}

func (h *terminalBackpressureHarness) openStream(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, err := h.device.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var envelope protocol.Envelope
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.StreamOpen == nil {
		t.Fatalf("打开流: %s, %v", payload, err)
	}
	return envelope.StreamOpen.StreamID
}

func (h *terminalBackpressureHarness) assertStopped(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, err := h.device.Read(ctx)
	if err != nil {
		t.Fatalf("未收到 stream.close: %v", err)
	}
	var envelope protocol.Envelope
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.StreamClose == nil || envelope.StreamClose.StreamID != id {
		t.Fatalf("关闭流: %s, %v", payload, err)
	}
	terminalWait(t, h.finished, "终端处理器退出")
	h.connector.mu.Lock()
	defer h.connector.mu.Unlock()
	if len(h.connector.streams) != 0 {
		t.Fatalf("流未清理: %d", len(h.connector.streams))
	}
}

func (h *terminalBackpressureHarness) output(id string, data []byte) {
	h.connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: id, Data: data}})
}

func TestTerminalSlowBrowserWriteTimeout(t *testing.T) {
	h := newTerminalBackpressureHarness(t, nil, nil)
	id := h.openStream(t)
	data := make([]byte, 1024)
	h.output(id, data)
	terminalWait(t, h.browserWrites, "浏览器写入被背压阻塞")
	for range 64 {
		h.output(id, data)
	}
	h.connector.mu.Lock()
	queued := len(h.connector.streams[id])
	h.connector.mu.Unlock()
	if queued != 64 {
		t.Fatalf("输出队列未填满: %d", queued)
	}
	// 即使 stream.close 尚未排到主循环，写超时也必须释放整个流。
	h.assertStopped(t, id)
}

func TestTerminalRevokedDuringSlowWrite(t *testing.T) {
	var revoked atomic.Bool
	h := newTerminalBackpressureHarness(t, func(handler *TerminalHandler) {
		handler.WriteTimeout = 30 * time.Second
		handler.ValidateGrant = func(context.Context, TerminalGrant) bool { return !revoked.Load() }
	}, nil)
	id := h.openStream(t)
	h.output(id, []byte("blocked"))
	terminalWait(t, h.browserWrites, "慢写开始")
	revoked.Store(true)
	// 期限远大于断言等待时间，证明不是依靠写超时或主 select 空闲。
	h.assertStopped(t, id)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := h.browser.Read(ctx)
	if err == nil || ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		t.Fatalf("撤权慢连接关闭结果不合理: %v", err)
	}
}

func TestTerminalRevokedDuringOpenStream(t *testing.T) {
	var revoked atomic.Bool
	h := newTerminalBackpressureHarness(t, func(handler *TerminalHandler) {
		handler.ValidateGrant = func(context.Context, TerminalGrant) bool { return !revoked.Load() }
	}, nil)
	// 设备不读，OpenStream 的真实 WebSocket 写入无法完成。
	terminalWait(t, h.deviceWrites, "OpenStream 写入开始")
	revoked.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := h.browser.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("打开流期间撤权应为 1008: %v", err)
	}
	// Manager 的共享 Connector 写使用 WithoutCancel，不能由终端取消。
	// 浏览器必须先收到撤权关闭；放行该在途写后，终端不得遗留打开的流。
	id := h.openStream(t)
	h.assertStopped(t, id)
}

func TestTerminalSlowApplicationPongTimeout(t *testing.T) {
	h := newTerminalBackpressureHarness(t, nil, nil)
	id := h.openStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.browser.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","id":"slow"}`)); err != nil {
		t.Fatal(err)
	}
	terminalWait(t, h.browserWrites, "应用 pong 写入开始")
	h.assertStopped(t, id)
}

func TestTerminalGrantTimeoutFailsClosed(t *testing.T) {
	var calls atomic.Int32
	h := newTerminalBackpressureHarness(t, func(handler *TerminalHandler) {
		handler.validationTimeout = 30 * time.Millisecond
		handler.ValidateGrant = func(ctx context.Context, _ TerminalGrant) bool {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("权限查询缺少期限")
			}
			if calls.Add(1) == 1 {
				return true
			}
			<-ctx.Done()
			return true // 查询超时，迟到的 true 也不能保留权限。
		}
	}, nil)
	id := h.openStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := h.browser.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("权限查询超时应为 1008: %v", err)
	}
	h.assertStopped(t, id)
}

func TestTerminalInitialGrantTimeoutFailsClosed(t *testing.T) {
	manager := NewManager()
	manager.Register("device", nil)
	handler := NewTerminalHandler(manager)
	handler.validationTimeout = 20 * time.Millisecond
	handler.ValidateGrant = func(ctx context.Context, _ TerminalGrant) bool {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("初次权限查询缺少期限")
		}
		<-ctx.Done()
		return true
	}
	hash := sha256.Sum256([]byte("session-secret"))
	ticket, err := handler.IssueWithGrant("device", "work", TerminalGrant{SessionHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/ws/v1/terminal", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "sinthmux.v1, sinthmux.ticket."+ticket)
	r.AddCookie(&http.Cookie{Name: "sinthmux_session", Value: "session-secret"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("初次查询超时状态码: %d", w.Code)
	}
}

func TestTerminalNormalSessionCloseAndPing(t *testing.T) {
	pinged := make(chan struct{}, 1)
	h := newTerminalBackpressureHarness(t, func(handler *TerminalHandler) {
		handler.PingInterval = 20 * time.Millisecond
	}, &websocket.DialOptions{OnPingReceived: func(context.Context, []byte) bool {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return true
	}})
	id := h.openStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.output(id, []byte("screen"))
	kind, data, err := h.browser.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(data) != "screen" {
		t.Fatalf("正常输出: %v %q %v", kind, data, err)
	}
	if err := h.browser.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","id":"17"}`)); err != nil {
		t.Fatal(err)
	}
	kind, data, err = h.browser.Read(ctx)
	if err != nil || kind != websocket.MessageText || string(data) != `{"id":"17","type":"pong"}` {
		t.Fatalf("应用 pong: %v %q %v", kind, data, err)
	}
	readDone := make(chan error, 1)
	go func() {
		kind, data, err := h.browser.Read(ctx)
		if err == nil && (kind != websocket.MessageBinary || string(data) != "last output") {
			err = errors.New("关闭前的末尾输出不匹配")
		}
		if err == nil {
			_, _, err = h.browser.Read(ctx)
		}
		readDone <- err
	}()
	terminalWait(t, pinged, "协议 ping")
	h.output(id, []byte("last output"))
	h.connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id, Reason: "session ended"}})
	err = <-readDone
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.StatusNormalClosure || closeErr.Reason != "session ended" {
		t.Fatalf("正常 session 关闭: %v", err)
	}
	h.assertStopped(t, id)
}

func TestTerminalDisconnectCancelsValidation(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	h := newTerminalBackpressureHarness(t, func(handler *TerminalHandler) {
		handler.ValidateGrant = func(ctx context.Context, _ TerminalGrant) bool {
			if calls.Add(1) == 1 {
				return true
			}
			close(started)
			<-ctx.Done()
			close(finished)
			return false
		}
	}, nil)
	id := h.openStream(t)
	terminalWait(t, started, "权限复核开始")
	_ = h.browser.CloseNow()
	terminalWait(t, finished, "连接退出取消权限查询")
	h.assertStopped(t, id)
}

func TestTerminalCloseHandshakeHasHardDeadline(t *testing.T) {
	h := newTerminalBackpressureHarness(t, nil, nil)
	id := h.openStream(t)
	h.connector.streamEvent(protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id, Reason: "done"}})
	terminalWait(t, h.browserWrites, "Close 帧写入被阻塞")
	// Close 已开始且无活跃应用写，硬关闭仍须突破 closing 标记。
	h.assertStopped(t, id)
}
