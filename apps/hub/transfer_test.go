package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// fakeDevice answers transfer RPCs in memory the way the connector does.
type fakeDevice struct {
	mu        sync.Mutex
	uploads   map[string][]byte
	committed map[string][]byte
	aborted   int
	history   []byte
	chunks    int
	failChunk bool
	cleared   []string
}

func (d *fakeDevice) answer(request protocol.RPCRequest) protocol.RPCResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	transfer := request.Transfer
	switch request.Method {
	case "file.upload.begin":
		id := "u" + string(rune('0'+len(d.uploads)))
		d.uploads[id] = make([]byte, transfer.Size)
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: id}}
	case "file.upload.chunk":
		d.chunks++
		if d.failChunk {
			return protocol.RPCResponse{ErrorCode: "upload_unavailable", Error: "disk full"}
		}
		copy(d.uploads[transfer.ID][transfer.Offset:], transfer.Data)
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: transfer.ID}}
	case "file.upload.commit":
		data := d.uploads[transfer.ID]
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != transfer.SHA256 {
			return protocol.RPCResponse{ErrorCode: "checksum_mismatch", Error: "checksum"}
		}
		d.committed[transfer.ID] = data
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: transfer.ID, Path: "/home/me/.sinthmux/uploads/x-" + transfer.ID, FileName: "x-" + transfer.ID, Size: int64(len(data))}}
	case "file.upload.abort":
		d.aborted++
		delete(d.uploads, transfer.ID)
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: transfer.ID}}
	case "terminal.export.begin":
		if request.Name != "work" {
			return protocol.RPCResponse{ErrorCode: "not_found", Error: "session not found"}
		}
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: "e1", Size: int64(len(d.history))}}
	case "terminal.export.read":
		end := min(transfer.Offset+protocol.TransferChunkSize, int64(len(d.history)))
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: "e1", Data: d.history[transfer.Offset:end], EOF: end == int64(len(d.history))}}
	case "terminal.export.close":
		return protocol.RPCResponse{OK: true, Transfer: &protocol.TransferResult{ID: transfer.ID}}
	case "session.notify.clear":
		d.cleared = append(d.cleared, request.Name)
		return protocol.RPCResponse{OK: true}
	}
	return protocol.RPCResponse{ErrorCode: "unsupported_method", Error: "unsupported RPC method"}
}

func startTransferHub(t *testing.T, device *fakeDevice, capabilities []string) (*httptest.Server, *devices.Registry) {
	t.Helper()
	manager := relay.NewManager()
	registry := devices.NewRegistry()
	api := newTransferHandler(manager, registry)
	router := chi.NewRouter()
	router.Post("/api/v1/devices/{deviceId}/uploads", api.upload)
	router.Get("/api/v1/devices/{deviceId}/sessions/{sessionName}/scrollback", api.exportHistory)
	router.Delete("/api/v1/devices/{deviceId}/sessions/{sessionName}/notification", api.clearNotification)
	router.Handle("/ws", relay.ConnectorHandler{Registry: registry, Manager: manager, DevToken: "test"})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(64 << 10)
	var writeMu sync.Mutex
	write := func(envelope protocol.Envelope) {
		payload, _ := json.Marshal(envelope)
		if len(payload) > 64<<10 {
			t.Errorf("message of %d bytes exceeds the 64 KiB read limit", len(payload))
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.Write(ctx, websocket.MessageText, payload)
	}
	write(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: "dev", Capabilities: capabilities}})
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			_, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var envelope protocol.Envelope
			if json.Unmarshal(payload, &envelope) != nil || envelope.Type != protocol.MessageRPCRequest {
				continue
			}
			go func() {
				response := device.answer(*envelope.Request)
				write(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: envelope.RequestID, Response: &response})
			}()
		}
	}()
	return server, registry
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{uploads: map[string][]byte{}, committed: map[string][]byte{}}
}

var allCapabilities = []string{protocol.CapabilityFileUpload, protocol.CapabilityTerminalExport, protocol.CapabilitySessionNotify}

func TestUploadStreamsChunksAndVerifiesChecksum(t *testing.T) {
	device := newFakeDevice()
	server, _ := startTransferHub(t, device, allCapabilities)
	data := make([]byte, 5*protocol.TransferChunkSize+123)
	_, _ = rand.Read(data)
	response, err := http.Post(server.URL+"/api/v1/devices/dev/uploads?name=shot.png", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	_ = json.NewDecoder(response.Body).Decode(&body)
	if response.StatusCode != http.StatusCreated || body.Size != int64(len(data)) || !strings.Contains(body.Path, "/.sinthmux/uploads/") {
		t.Fatalf("status=%d body=%+v", response.StatusCode, body)
	}
	if device.chunks != 6 || !bytes.Equal(device.committed["u0"], data) {
		t.Fatalf("chunks=%d committed match=%v", device.chunks, bytes.Equal(device.committed["u0"], data))
	}
}

func TestUploadRejectsBadRequests(t *testing.T) {
	device := newFakeDevice()
	server, _ := startTransferHub(t, device, allCapabilities)
	post := func(body io.Reader, length int64) int {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/devices/dev/uploads?name=a", body)
		request.ContentLength = length
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	// A declared size over the limit is refused before the body is read.
	raw, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(raw, "POST /api/v1/devices/dev/uploads?name=a HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", protocol.MaxUploadSize+1)
	status, _ := bufio.NewReader(raw).ReadString('\n')
	raw.Close()
	if !strings.Contains(status, "413") {
		t.Fatalf("oversized: %q", status)
	}
	if got := post(io.MultiReader(strings.NewReader("abc")), -1); got != http.StatusLengthRequired {
		t.Fatalf("chunked: %d", got)
	}
	if got := post(strings.NewReader(""), 0); got != http.StatusBadRequest {
		t.Fatalf("empty: %d", got)
	}
	device.failChunk = true
	if got := post(bytes.NewReader(make([]byte, 100)), 100); got != http.StatusServiceUnavailable {
		t.Fatalf("device failure: %d", got)
	}
	if device.aborted != 1 {
		t.Fatalf("failed upload aborted %d times", device.aborted)
	}
	response, _ := http.Post(server.URL+"/api/v1/devices/missing/uploads", "application/octet-stream", strings.NewReader("x"))
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline device: %d", response.StatusCode)
	}
}

func TestOldConnectorWithoutCapabilityGets501(t *testing.T) {
	server, _ := startTransferHub(t, newFakeDevice(), nil)
	for _, request := range []*http.Request{
		mustRequest(http.MethodPost, server.URL+"/api/v1/devices/dev/uploads?name=a", "x"),
		mustRequest(http.MethodGet, server.URL+"/api/v1/devices/dev/sessions/work/scrollback", ""),
		mustRequest(http.MethodDelete, server.URL+"/api/v1/devices/dev/sessions/work/notification", ""),
	} {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s %s: %d", request.Method, request.URL.Path, response.StatusCode)
		}
	}
}

func mustRequest(method, url, body string) *http.Request {
	request, _ := http.NewRequest(method, url, strings.NewReader(body))
	return request
}

func TestExportStreamsHistoryInOrder(t *testing.T) {
	device := newFakeDevice()
	var history strings.Builder
	for i := 0; i < 20000; i++ {
		history.WriteString("line ")
		history.WriteString(strings.Repeat("中", i%7))
		history.WriteString("\n")
	}
	device.history = []byte(history.String())
	server, _ := startTransferHub(t, device, allCapabilities)
	response, err := http.Get(server.URL + "/api/v1/devices/dev/sessions/work/scrollback?lines=all")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Equal(got, device.history) || response.Header.Get("X-Sinthmux-Export-Size") != strconv.Itoa(len(device.history)) || response.Header.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment; filename*=UTF-8''work-") {
		t.Fatalf("status=%d len=%d want=%d headers=%v", response.StatusCode, len(got), len(device.history), response.Header)
	}
	for path, want := range map[string]int{"/sessions/missing/scrollback": 404, "/sessions/work/scrollback?lines=abc": 400, "/sessions/bad.name/scrollback": 400} {
		response, _ := http.Get(server.URL + "/api/v1/devices/dev" + path)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: %d want %d", path, response.StatusCode, want)
		}
	}
}

func TestEmptyExportAndNotificationClear(t *testing.T) {
	device := newFakeDevice()
	server, registry := startTransferHub(t, device, allCapabilities)
	response, _ := http.Get(server.URL + "/api/v1/devices/dev/sessions/work/scrollback")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("empty export: %d %q", response.StatusCode, body)
	}
	response, _ = http.DefaultClient.Do(mustRequest(http.MethodDelete, server.URL+"/api/v1/devices/dev/sessions/work/notification", ""))
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(device.cleared) != 1 || device.cleared[0] != "work" {
		t.Fatalf("clear: %d %v", response.StatusCode, device.cleared)
	}
	registry.SetNotifications("dev", []protocol.SessionNotification{{Session: "work", Color: "purple", Message: "done", At: 1}, {Session: "bad name", Color: "red"}})
	items := registry.List()[0].Notifications
	if len(items) != 1 || items[0].Color != "blue" {
		t.Fatalf("notifications: %+v", items)
	}
}

func TestUploadLimitsConcurrencyPerDevice(t *testing.T) {
	api := newTransferHandler(relay.NewManager(), devices.NewRegistry())
	if !api.acquireUpload("d") || !api.acquireUpload("d") || api.acquireUpload("d") {
		t.Fatal("expected two concurrent uploads per device")
	}
	api.releaseUpload("d")
	if !api.acquireUpload("d") {
		t.Fatal("slot not released")
	}
	_ = time.Second
}
