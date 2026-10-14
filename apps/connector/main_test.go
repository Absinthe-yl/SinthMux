package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func testRPCRequest(id string) protocol.Envelope {
	return protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: id, Request: &protocol.RPCRequest{Method: "test.block"}}
}

func testRPCSuccess(request protocol.Envelope) protocol.Envelope {
	return protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: request.RequestID, Response: &protocol.RPCResponse{OK: true, Name: request.RequestID}}
}

func checkBusyResponse(t *testing.T, response protocol.Envelope, id string) {
	t.Helper()
	if response.Version != protocol.Version || response.Type != protocol.MessageRPCResponse || response.RequestID != id || response.Response == nil {
		t.Fatalf("busy 响应信封错误: %+v", response)
	}
	if response.Response.OK || response.Response.ErrorCode != "busy" || response.Response.Error == "" || response.Response.Name != "" || response.Response.Transfer != nil || len(response.Response.Sessions) != 0 {
		t.Fatalf("busy 响应内容错误: %+v", response.Response)
	}
}

func TestRPCDispatcherBoundsOverloadAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var executed, exited atomic.Int32
		writes := make(chan protocol.Envelope, 1)
		d := newRPCDispatcher(ctx, make(chan struct{}, rpcConcurrency), func(response protocol.Envelope) error {
			if response.Response.ErrorCode == "busy" {
				writes <- response
				// 模拟等待发送锁或网络；分发不应等待这个发送函数。
				<-ctx.Done()
			}
			return ctx.Err()
		}, func(ctx context.Context, request protocol.Envelope) protocol.Envelope {
			executed.Add(1)
			<-ctx.Done()
			exited.Add(1)
			return testRPCSuccess(request)
		}, func(error) {})
		for i := 0; i < rpcConcurrency; i++ {
			if err := d.dispatch(testRPCRequest(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if got := executed.Load(); got != 8 {
			t.Fatalf("执行数 = %d，期望 8", got)
		}
		if err := d.dispatch(testRPCRequest("busy-in-flight")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		checkBusyResponse(t, <-writes, "busy-in-flight")
		for i := 0; i < rpcOverloadQueueSize; i++ {
			if err := d.dispatch(testRPCRequest(fmt.Sprintf("queued-%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.dispatch(testRPCRequest("overflow")); err == nil || !strings.Contains(err.Error(), "过载响应队列已满") {
			t.Fatalf("队列满时未明确失败: %v", err)
		}
		if got := len(d.overload); got != rpcOverloadQueueSize || executed.Load() != 8 {
			t.Fatalf("队列或执行数越界: queued=%d executed=%d", got, executed.Load())
		}
		cancel()
		d.workers.Wait()
		<-d.writerDone
		if exited.Load() != 8 || len(d.slots) != 0 {
			t.Fatalf("取消后仍有 RPC: exited=%d slots=%d", exited.Load(), len(d.slots))
		}
		if err := d.dispatch(testRPCRequest("after-cancel")); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后仍接收请求: %v", err)
		}
	})
}

func TestRPCDispatcherPreservesResponsesAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		releaseRPC, releaseWrite := make(chan struct{}), make(chan struct{})
		responses := make(chan protocol.Envelope, 16)
		var executed atomic.Int32
		d := newRPCDispatcher(ctx, make(chan struct{}, rpcConcurrency), func(response protocol.Envelope) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseWrite:
				responses <- response
				return nil
			}
		}, func(ctx context.Context, request protocol.Envelope) protocol.Envelope {
			executed.Add(1)
			select {
			case <-ctx.Done():
			case <-releaseRPC:
			}
			return testRPCSuccess(request)
		}, func(err error) { t.Errorf("意外发送失败: %v", err) })
		for i := 0; i < 8; i++ {
			if err := d.dispatch(testRPCRequest(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		close(releaseRPC)
		synctest.Wait()
		if len(d.slots) != 8 {
			t.Fatal("正常响应发送前提前释放槽位")
		}
		if err := d.dispatch(testRPCRequest("busy")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if executed.Load() != 8 {
			t.Fatal("过载请求被执行")
		}
		close(releaseWrite)
		synctest.Wait()
		seen := make(map[string]bool)
		for i := 0; i < 9; i++ {
			response := <-responses
			if seen[response.RequestID] {
				t.Fatalf("重复响应: %s", response.RequestID)
			}
			seen[response.RequestID] = true
			if response.RequestID == "busy" {
				checkBusyResponse(t, response, "busy")
			} else if response.Response == nil || !response.Response.OK || response.Response.Name != response.RequestID {
				t.Fatalf("正常 RPC 响应错误: %+v", response)
			}
		}
		for i := 0; i < 8; i++ {
			if !seen[fmt.Sprint(i)] {
				t.Fatalf("丢失请求 %d 的响应", i)
			}
		}
		if !seen["busy"] || len(d.slots) != 0 {
			t.Fatal("过载回包丢失或槽位未释放")
		}
		if err := d.dispatch(testRPCRequest("retry")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if response := <-responses; response.RequestID != "retry" || !response.Response.OK || executed.Load() != 9 {
			t.Fatalf("恢复后请求失败: %+v", response)
		}
		cancel()
		d.workers.Wait()
		<-d.writerDone
	})
}

func TestRPCDispatcherReportsWriteFailure(t *testing.T) {
	for _, overloaded := range []bool{false, true} {
		t.Run(fmt.Sprintf("overloaded=%t", overloaded), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				want := errors.New("测试发送失败")
				failures := make(chan error, 16)
				d := newRPCDispatcher(ctx, make(chan struct{}, rpcConcurrency), func(protocol.Envelope) error { return want }, func(ctx context.Context, request protocol.Envelope) protocol.Envelope {
					if overloaded {
						<-ctx.Done()
					}
					return testRPCSuccess(request)
				}, func(err error) { failures <- err })
				count := 1
				if overloaded {
					count = 9
				}
				for i := 0; i < count; i++ {
					if err := d.dispatch(testRPCRequest(fmt.Sprint(i))); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				if err := <-failures; !errors.Is(err, want) {
					t.Fatalf("发送错误未传递: %v", err)
				}
				cancel()
				d.workers.Wait()
				<-d.writerDone
			})
		})
	}
}

func TestRPCDispatcherDropsCanceledResults(t *testing.T) {
	for _, ignoreContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignoreContext=%t", ignoreContext), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				releaseAll := sync.OnceFunc(func() { close(release) })
				defer releaseAll()
				var executed atomic.Int32
				responses := make(chan protocol.Envelope, 8)
				d := newRPCDispatcher(ctx, make(chan struct{}, rpcConcurrency), func(response protocol.Envelope) error {
					responses <- response
					return nil
				}, func(ctx context.Context, request protocol.Envelope) protocol.Envelope {
					executed.Add(1)
					if ignoreContext {
						<-release
					} else {
						<-ctx.Done()
					}
					return testRPCSuccess(request)
				}, func(err error) { t.Errorf("意外失败: %v", err) })
				for i := 0; i < 8; i++ {
					if err := d.dispatch(testRPCRequest(fmt.Sprint(i))); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				if executed.Load() != 8 {
					t.Fatal("未启动全部 RPC")
				}
				cancel()
				<-d.writerDone
				synctest.Wait()
				if ignoreContext && len(d.slots) != 8 {
					t.Fatal("取消时提前释放了仍执行中的 RPC 槽位")
				}
				releaseAll()
				d.workers.Wait()
				if len(d.slots) != 0 || len(responses) != 0 {
					t.Fatalf("槽位未释放或取消后的结果仍被发送: slots=%d responses=%d", len(d.slots), len(responses))
				}
			})
		})
	}
}

// 调用方先停止分发或暂停发请求；占齐槽位构成执行/发送结束屏障，不轮询或 sleep。
func waitForRPCSlots(t *testing.T, slots chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	acquired := 0
	defer func() {
		for i := 0; i < acquired; i++ {
			<-slots
		}
	}()
	for acquired < cap(slots) {
		select {
		case slots <- struct{}{}:
			acquired++
		case <-ctx.Done():
			t.Fatal("RPC 未归还全部槽位")
		}
	}
}

type rpcRunPeer struct {
	ctx    context.Context
	cancel context.CancelFunc
	hub    *websocket.Conn
	done   chan struct{}
	err    error
}

func startRPCRun(t *testing.T, parent context.Context, slots chan struct{}, execute func(context.Context, protocol.Envelope) protocol.Envelope) *rpcRunPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	peer := &rpcRunPeer{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		accepted <- conn
		<-ctx.Done()
	}))
	t.Cleanup(server.Close)
	settings := config.Connector{HubURL: "ws" + strings.TrimPrefix(server.URL, "http"), DevToken: "test", DeviceID: "test", Name: "test"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		defer close(peer.done)
		peer.err = runWithRPC(ctx, &settings, false, logger, nil, slots, execute)
	}()
	t.Cleanup(func() {
		cancel()
		if peer.hub != nil {
			_ = peer.hub.CloseNow()
		}
		select {
		case <-peer.done:
		case <-time.After(3 * time.Second):
			t.Error("放行 RPC 后 run 仍未退出")
		}
	})
	select {
	case peer.hub = <-accepted:
	case <-peer.done:
		t.Fatalf("run 未建立连接: %v", peer.err)
	case <-parent.Done():
		t.Fatal("连接未建立")
	}
	if hello := peer.next(t); hello.Type != protocol.MessageConnectorHello || hello.Version != protocol.Version {
		t.Fatalf("hello 错误: %+v", hello)
	}
	return peer
}

func (p *rpcRunPeer) send(t *testing.T, id string) {
	t.Helper()
	if err := write(p.ctx, p.hub, testRPCRequest(id)); err != nil {
		t.Fatal(err)
	}
}

func (p *rpcRunPeer) next(t *testing.T) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, time.Second)
	defer cancel()
	kind, data, err := p.hub.Read(ctx)
	if err != nil {
		t.Fatalf("读取 RPC 响应失败: %v", err)
	}
	var response protocol.Envelope
	if kind != websocket.MessageText || json.Unmarshal(data, &response) != nil {
		t.Fatal("非法 JSON 响应")
	}
	return response
}

func (p *rpcRunPeer) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
		if p.err == nil {
			t.Fatal("run 取消后未报告错误")
		}
	case <-time.After(time.Second):
		t.Fatal("run 取消后仍在等待忽略 ctx 的 RPC")
	}
}

func TestRunRPCGateSurvivesReconnectWithUncancelableIO(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	slots := make(chan struct{}, rpcConcurrency)
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	// 即使中途断言失败，也先放行不可取消的执行器，再回收所有连接和槽位。
	defer releaseAll()
	t.Cleanup(func() { waitForRPCSlots(t, slots) })
	started := make(chan string, 32)
	var active, executed atomic.Int32
	var exceeded atomic.Bool
	execute := func(_ context.Context, request protocol.Envelope) protocol.Envelope {
		if active.Add(1) > 8 {
			exceeded.Store(true)
		}
		defer active.Add(-1)
		executed.Add(1)
		started <- request.RequestID
		<-release // 故意忽略 ctx，模拟 WriteAt/Sync 等 OS I/O 边界。
		return testRPCSuccess(request)
	}
	startFour := func(peer *rpcRunPeer, generation int) {
		t.Helper()
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("generation-%d-%d", generation, i)
			peer.send(t, id)
			select {
			case got := <-started:
				if got != id {
					t.Fatalf("执行了非预期请求: %s，期望 %s", got, id)
				}
			case <-ctx.Done():
				t.Fatal("RPC 未启动")
			}
		}
	}

	first := startRPCRun(t, ctx, slots, execute)
	startFour(first, 1)
	first.stop(t)
	if len(slots) != 4 || active.Load() != 4 {
		t.Fatal("第一代退出后未保留 4 个在途 I/O 槽位")
	}

	second := startRPCRun(t, ctx, slots, execute)
	startFour(second, 2)
	second.send(t, "second-busy")
	checkBusyResponse(t, second.next(t), "second-busy")
	second.stop(t)
	if len(slots) != 8 || active.Load() != 8 {
		t.Fatal("跨两代应恰好保留 8 个在途 I/O")
	}

	third := startRPCRun(t, ctx, slots, execute)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("third-busy-%d", i)
		third.send(t, id)
		checkBusyResponse(t, third.next(t), id)
	}
	if executed.Load() != 8 || exceeded.Load() {
		t.Fatalf("重连绕过了进程级上限: executed=%d", executed.Load())
	}

	releaseAll()
	waitForRPCSlots(t, slots)
	if active.Load() != 0 || len(slots) != 0 {
		t.Fatal("放行旧代 I/O 后槽位未全部归还")
	}
	third.send(t, "recovered")
	response := third.next(t)
	if response.Version != protocol.Version || response.Type != protocol.MessageRPCResponse || response.RequestID != "recovered" || response.Response == nil || !response.Response.OK || response.Response.Name != "recovered" {
		t.Fatalf("新代收到旧响应或恢复失败: %+v", response)
	}
	third.stop(t)
	waitForRPCSlots(t, slots)
	if executed.Load() != 9 || active.Load() != 0 || exceeded.Load() {
		t.Fatalf("执行数量或回收结果错误: executed=%d active=%d", executed.Load(), active.Load())
	}
}

func TestRunRPCOverloadKeepsReadingAndRecovers(t *testing.T) {
	for _, cancelBusy := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelBusy=%t", cancelBusy), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			accepted := make(chan *websocket.Conn, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				accepted <- conn
				<-ctx.Done()
			}))
			t.Cleanup(server.Close)
			started := make(chan string, 16)
			release := make(chan struct{})
			var exited atomic.Int32
			slots := make(chan struct{}, rpcConcurrency)
			settings := config.Connector{HubURL: "ws" + strings.TrimPrefix(server.URL, "http"), DevToken: "test", DeviceID: "test", Name: "test"}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			runDone := make(chan struct{})
			var runErr error
			go func() {
				defer close(runDone)
				runErr = runWithRPC(ctx, &settings, false, logger, nil, slots, func(ctx context.Context, request protocol.Envelope) protocol.Envelope {
					started <- request.RequestID
					defer exited.Add(1)
					select {
					case <-ctx.Done():
					case <-release:
					}
					return testRPCSuccess(request)
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-runDone:
					waitForRPCSlots(t, slots)
				case <-time.After(3 * time.Second):
					t.Error("取消后 run 未退出")
				}
			})
			var hub *websocket.Conn
			select {
			case hub = <-accepted:
			case <-ctx.Done():
				t.Fatal("连接未建立")
			}
			replies := make(chan protocol.Envelope, 32)
			readErrors := make(chan error, 1)
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				for {
					kind, data, err := hub.Read(ctx)
					if err != nil {
						readErrors <- err
						return
					}
					var response protocol.Envelope
					if kind != websocket.MessageText || json.Unmarshal(data, &response) != nil {
						readErrors <- errors.New("收到非法 JSON 文本响应")
						return
					}
					if response.Response != nil && response.Response.ErrorCode == "busy" && !strings.Contains(string(data), `"ok":false`) {
						readErrors <- errors.New("busy 回包未显式编码 ok=false")
						return
					}
					select {
					case replies <- response:
					case <-ctx.Done():
						return
					}
				}
			}()
			t.Cleanup(func() {
				cancel()
				_ = hub.CloseNow()
				select {
				case <-readDone:
				case <-time.After(3 * time.Second):
					t.Error("测试 Hub 的读协程未退出")
				}
			})
			next := func() protocol.Envelope {
				t.Helper()
				select {
				case response := <-replies:
					return response
				case err := <-readErrors:
					t.Fatalf("读取回包失败: %v", err)
				case <-ctx.Done():
					t.Fatal("等待回包超时")
				}
				return protocol.Envelope{}
			}
			send := func(envelope protocol.Envelope) {
				t.Helper()
				if err := write(ctx, hub, envelope); err != nil {
					t.Fatal(err)
				}
			}
			if hello := next(); hello.Type != protocol.MessageConnectorHello || hello.Version != protocol.Version {
				t.Fatalf("hello 错误: %+v", hello)
			}
			for i := 0; i < 8; i++ {
				id := fmt.Sprint(i)
				send(testRPCRequest(id))
				select {
				case got := <-started:
					if got != id {
						t.Fatalf("执行请求 %s，期望 %s", got, id)
					}
				case <-ctx.Done():
					t.Fatal("8 个并发 RPC 未全部启动")
				}
			}
			send(testRPCRequest("ninth"))
			// 不创建真实 PTY：发送流输入/控制，再用非法 open 的 close 回包确认读循环进展。
			send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: "unused", Data: []byte("input")}})
			send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamResize, StreamResize: &protocol.StreamResize{StreamID: "unused", Cols: 80, Rows: 24}})
			send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: "unused"}})
			send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamOpen, StreamOpen: &protocol.StreamOpen{StreamID: "invalid", Session: "invalid session", Cols: 80, Rows: 24}})
			busy, control := false, false
			for i := 0; i < 2; i++ {
				response := next()
				switch response.Type {
				case protocol.MessageRPCResponse:
					checkBusyResponse(t, response, "ninth")
					busy = true
				case protocol.MessageStreamClose:
					control = response.StreamClose != nil && response.StreamClose.StreamID == "invalid" && response.StreamClose.Reason != ""
				default:
					t.Fatalf("意外响应: %+v", response)
				}
			}
			if !busy || !control {
				t.Fatal("busy 或流控制响应缺失")
			}
			pingCtx, cancelPing := context.WithTimeout(ctx, time.Second)
			err := hub.Ping(pingCtx)
			cancelPing()
			if err != nil {
				t.Fatalf("RPC 过载阻塞了 ping/pong: %v", err)
			}
			select {
			case id := <-started:
				t.Fatalf("过载请求被执行: %s", id)
			default:
			}
			if cancelBusy {
				cancel()
				select {
				case <-runDone:
					waitForRPCSlots(t, slots)
					if runErr == nil || exited.Load() != 8 {
						t.Fatalf("取消后任务未回收: err=%v exited=%d", runErr, exited.Load())
					}
				case <-time.After(3 * time.Second):
					t.Fatal("取消未回收 run 和阻塞 RPC")
				}
				return
			}
			close(release)
			seen := make(map[string]bool)
			for i := 0; i < 8; i++ {
				response := next()
				if response.Version != protocol.Version || response.Type != protocol.MessageRPCResponse || response.Response == nil || !response.Response.OK || response.Response.Name != response.RequestID || seen[response.RequestID] {
					t.Fatalf("正常响应丢失、重复或失败: %+v", response)
				}
				seen[response.RequestID] = true
			}
			for i := 0; i < 8; i++ {
				if !seen[fmt.Sprint(i)] {
					t.Fatalf("缺失请求 %d 的响应", i)
				}
			}
			// 对端收到回包可能略早于本地发送函数返回；按回包重试，不靠 sleep 判断槽位释放。
			for attempt := 0; ; attempt++ {
				id := fmt.Sprintf("retry-%d", attempt)
				send(testRPCRequest(id))
				response := next()
				if response.RequestID != id || response.Response == nil {
					t.Fatalf("重试响应错误: %+v", response)
				}
				if response.Response.OK {
					if response.Response.Name != id {
						t.Fatal("恢复请求的执行结果错误")
					}
					break
				}
				checkBusyResponse(t, response, id)
			}
		})
	}
}
