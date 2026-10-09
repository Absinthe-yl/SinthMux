package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

const (
	transferWindow       = 4
	exportWindow         = 8
	maxUploadsPerDevice  = 2
	maxExportLinesOption = 1_000_000
)

// transferHandler moves files to a device (uploads) and terminal history from
// it (exports) as a series of bounded RPCs over the connector connection.
type transferHandler struct {
	manager  *relay.Manager
	registry *devices.Registry

	mu      sync.Mutex
	uploads map[string]int
}

func newTransferHandler(manager *relay.Manager, registry *devices.Registry) *transferHandler {
	return &transferHandler{manager: manager, registry: registry, uploads: map[string]int{}}
}

// rpcStatus maps connector error codes to HTTP status codes.
func rpcStatus(code string) int {
	switch code {
	case "invalid_name", "invalid_request":
		return http.StatusBadRequest
	case "not_found":
		return http.StatusNotFound
	case "already_exists":
		return http.StatusConflict
	case "tmux_unavailable", "upload_unavailable":
		return http.StatusServiceUnavailable
	case "timeout":
		return http.StatusGatewayTimeout
	case "unsupported_method":
		return http.StatusNotImplemented
	case "too_large":
		return http.StatusRequestEntityTooLarge
	case "checksum_mismatch":
		return http.StatusUnprocessableEntity
	case "busy":
		return http.StatusTooManyRequests
	case "transfer_not_found":
		return http.StatusGone
	}
	return http.StatusBadGateway
}

type rpcFailure struct {
	status  int
	message string
}

func (e *rpcFailure) Error() string { return e.message }

func callError(err error) *rpcFailure {
	switch {
	case errors.Is(err, relay.ErrOffline):
		return &rpcFailure{http.StatusServiceUnavailable, err.Error()}
	case errors.Is(err, relay.ErrTimeout):
		return &rpcFailure{http.StatusGatewayTimeout, err.Error()}
	}
	return &rpcFailure{http.StatusBadGateway, err.Error()}
}

// transfer runs one transfer RPC and returns its result or an HTTP-ready error.
func (h *transferHandler) transfer(ctx context.Context, deviceID string, request protocol.RPCRequest) (*protocol.TransferResult, *rpcFailure) {
	response, err := h.manager.Call(ctx, deviceID, request)
	if err != nil {
		return nil, callError(err)
	}
	if !response.OK {
		message := response.Error
		if response.ErrorCode == "unsupported_method" {
			message = "设备代理版本过旧，不支持此功能；请在设备上重新运行接入命令"
		}
		return nil, &rpcFailure{rpcStatus(response.ErrorCode), message}
	}
	if response.Transfer == nil {
		return nil, &rpcFailure{http.StatusBadGateway, "invalid connector response"}
	}
	return response.Transfer, nil
}

func (h *transferHandler) requireCapability(w http.ResponseWriter, deviceID, capability string) bool {
	if !h.manager.Online(deviceID) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "device is offline"})
		return false
	}
	if !h.registry.HasCapability(deviceID, capability) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "设备代理版本过旧，不支持此功能；请在设备上重新运行接入命令"})
		return false
	}
	return true
}

func (h *transferHandler) acquireUpload(deviceID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uploads[deviceID] >= maxUploadsPerDevice {
		return false
	}
	h.uploads[deviceID]++
	return true
}

func (h *transferHandler) releaseUpload(deviceID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uploads[deviceID]--; h.uploads[deviceID] <= 0 {
		delete(h.uploads, deviceID)
	}
}

// upload handles POST /api/v1/devices/{deviceId}/uploads?name=<file name>
// with the raw file as the body. It streams 32 KiB chunks to the connector,
// up to four in flight, then commits with the SHA-256 the Hub computed.
func (h *transferHandler) upload(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceId")
	if !h.requireCapability(w, deviceID, protocol.CapabilityFileUpload) {
		return
	}
	size := r.ContentLength
	switch {
	case size < 0:
		writeJSON(w, http.StatusLengthRequired, map[string]string{"error": "Content-Length is required"})
		return
	case size == 0:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file is empty"})
		return
	case size > protocol.MaxUploadSize:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "文件超过 20 MiB 上限"})
		return
	}
	name := r.URL.Query().Get("name")
	if len(name) > 255 {
		name = name[:255]
	}
	if !h.acquireUpload(deviceID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "该设备已有上传在进行，请稍后重试"})
		return
	}
	defer h.releaseUpload(deviceID)

	ctx := r.Context()
	begin, failure := h.transfer(ctx, deviceID, protocol.RPCRequest{Method: "file.upload.begin", Transfer: &protocol.TransferRequest{FileName: name, Size: size}})
	if failure != nil {
		writeJSON(w, failure.status, map[string]string{"error": failure.message})
		return
	}
	committed := false
	defer func() {
		if !committed {
			abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = h.manager.Call(abortCtx, deviceID, protocol.RPCRequest{Method: "file.upload.abort", Transfer: &protocol.TransferRequest{ID: begin.ID}})
		}
	}()

	body := http.MaxBytesReader(w, r.Body, size)
	hash := sha256.New()
	slots := make(chan struct{}, transferWindow)
	var wait sync.WaitGroup
	var once sync.Once
	var firstFailure *rpcFailure
	chunkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(f *rpcFailure) {
		once.Do(func() { firstFailure = f; cancel() })
	}
	var offset int64
	for offset < size && chunkCtx.Err() == nil {
		length := min(int64(protocol.TransferChunkSize), size-offset)
		chunk := make([]byte, length)
		if _, err := io.ReadFull(body, chunk); err != nil {
			fail(&rpcFailure{http.StatusBadRequest, "upload body ended early"})
			break
		}
		hash.Write(chunk)
		select {
		case slots <- struct{}{}:
		case <-chunkCtx.Done():
		}
		if chunkCtx.Err() != nil {
			break
		}
		wait.Add(1)
		go func(offset int64, chunk []byte) {
			defer wait.Done()
			defer func() { <-slots }()
			if _, f := h.transfer(chunkCtx, deviceID, protocol.RPCRequest{Method: "file.upload.chunk", Transfer: &protocol.TransferRequest{ID: begin.ID, Offset: offset, Data: chunk}}); f != nil {
				fail(f)
			}
		}(offset, chunk)
		offset += length
	}
	wait.Wait()
	if firstFailure == nil && ctx.Err() != nil {
		firstFailure = &rpcFailure{http.StatusBadRequest, "upload cancelled"}
	}
	if firstFailure != nil {
		writeJSON(w, firstFailure.status, map[string]string{"error": firstFailure.message})
		return
	}
	result, failure := h.transfer(ctx, deviceID, protocol.RPCRequest{Method: "file.upload.commit", Transfer: &protocol.TransferRequest{ID: begin.ID, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}})
	if failure != nil {
		writeJSON(w, failure.status, map[string]string{"error": failure.message})
		return
	}
	committed = true
	writeJSON(w, http.StatusCreated, map[string]any{"path": result.Path, "fileName": result.FileName, "size": result.Size})
}

// exportHistory handles GET /api/v1/devices/{deviceId}/sessions/{sessionName}/scrollback?lines=1000|10000|all.
func (h *transferHandler) exportHistory(w http.ResponseWriter, r *http.Request) {
	deviceID, session := chi.URLParam(r, "deviceId"), chi.URLParam(r, "sessionName")
	if !protocol.ValidSessionName(session) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session name"})
		return
	}
	lines := 0
	if value := r.URL.Query().Get("lines"); value != "" && value != "all" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > maxExportLinesOption {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lines must be a positive number or all"})
			return
		}
		lines = parsed
	}
	if !h.requireCapability(w, deviceID, protocol.CapabilityTerminalExport) {
		return
	}
	ctx := r.Context()
	begin, failure := h.transfer(ctx, deviceID, protocol.RPCRequest{Method: "terminal.export.begin", Name: session, Transfer: &protocol.TransferRequest{Lines: lines}})
	if failure != nil {
		writeJSON(w, failure.status, map[string]string{"error": failure.message})
		return
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = h.manager.Call(closeCtx, deviceID, protocol.RPCRequest{Method: "terminal.export.close", Transfer: &protocol.TransferRequest{ID: begin.ID}})
	}()
	// Keep up to exportWindow reads in flight and write them in order: a
	// sliding window, so one slow chunk delays only itself, not a whole batch.
	type piece struct {
		data    []byte
		failure *rpcFailure
	}
	readCtx, cancelReads := context.WithCancel(ctx)
	defer cancelReads()
	read := func(offset int64) <-chan piece {
		result := make(chan piece, 1)
		go func() {
			response, f := h.transfer(readCtx, deviceID, protocol.RPCRequest{Method: "terminal.export.read", Transfer: &protocol.TransferRequest{ID: begin.ID, Offset: offset}})
			if f == nil && int64(len(response.Data)) != min(protocol.TransferChunkSize, begin.Size-offset) {
				f = &rpcFailure{http.StatusBadGateway, "short export chunk"}
			}
			if f != nil {
				result <- piece{failure: f}
				return
			}
			result <- piece{data: response.Data}
		}()
		return result
	}
	var inFlight []<-chan piece
	next := int64(0)
	fill := func() {
		for len(inFlight) < exportWindow && next < begin.Size {
			inFlight = append(inFlight, read(next))
			next += protocol.TransferChunkSize
		}
	}
	fill()
	// Wait for the first chunk before committing to a 200 so early failures
	// still produce a JSON error.
	var first piece
	if len(inFlight) > 0 {
		first = <-inFlight[0]
		inFlight = inFlight[1:]
		if first.failure != nil {
			writeJSON(w, first.failure.status, map[string]string{"error": first.failure.message})
			return
		}
		fill()
	}
	filename := session + "-" + time.Now().Format("20060102-150405") + ".txt"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename))
	w.Header().Set("Content-Length", strconv.FormatInt(begin.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(first.data); err != nil {
		return
	}
	for len(inFlight) > 0 {
		item := <-inFlight[0]
		inFlight = inFlight[1:]
		if item.failure != nil {
			return // headers are sent; a short body tells the client it failed
		}
		if _, err := w.Write(item.data); err != nil {
			return
		}
		fill()
	}
}

// clearNotification handles DELETE /api/v1/devices/{deviceId}/sessions/{sessionName}/notification.
func (h *transferHandler) clearNotification(w http.ResponseWriter, r *http.Request) {
	deviceID, session := chi.URLParam(r, "deviceId"), chi.URLParam(r, "sessionName")
	if !protocol.ValidSessionName(session) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session name"})
		return
	}
	if !h.requireCapability(w, deviceID, protocol.CapabilitySessionNotify) {
		return
	}
	response, err := h.manager.Call(r.Context(), deviceID, protocol.RPCRequest{Method: "session.notify.clear", Name: session})
	if err != nil {
		failure := callError(err)
		writeJSON(w, failure.status, map[string]string{"error": failure.message})
		return
	}
	if !response.OK {
		writeJSON(w, rpcStatus(response.ErrorCode), map[string]string{"error": response.Error})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
