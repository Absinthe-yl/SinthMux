package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

const (
	maxUploads         = 4
	maxExports         = 4
	maxExportSize      = 32 << 20
	transferIdleTime   = 2 * time.Minute
	uploadRetention    = 7 * 24 * time.Hour
	partialRetention   = time.Hour
	uploadCleanupEvery = 6 * time.Hour
)

// capabilities lists what this connector build supports; the Hub and Web UI
// show upload, export and notification controls only when advertised.
func capabilities() []string {
	result := []string{"device.info", "tmux.sessions.list", "tmux.sessions.manage", "terminal.stream", protocol.CapabilityFileUpload}
	if extendedTmux {
		result = append(result, protocol.CapabilityTerminalExport, protocol.CapabilitySessionNotify)
	}
	return result
}

type transferError = tmuxError

func transferFailure(code, message string) error { return &tmuxError{code: code, message: message} }

type upload struct {
	file      *os.File
	partPath  string
	finalPath string
	name      string
	size      int64
	written   map[int64]int
	received  int64
	touched   time.Time
}

type export struct {
	data    []byte
	touched time.Time
}

type transfers struct {
	mu      sync.Mutex
	uploads map[string]*upload
	exports map[string]*export
}

var activeTransfers = &transfers{uploads: map[string]*upload{}, exports: map[string]*export{}}

func transferID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// uploadDir returns ~/.sinthmux/uploads, creating it with owner-only access.
// A symlink in place of either directory is refused so uploads never escape it.
func uploadDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if root := os.Getenv("SINTHMUX_UPLOAD_DIR"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("SINTHMUX_UPLOAD_DIR must be absolute")
		}
		return root, ensurePrivateDir(root)
	}
	base := filepath.Join(home, ".sinthmux")
	if err := ensurePrivateDir(base); err != nil {
		return "", err
	}
	dir := filepath.Join(base, "uploads")
	return dir, ensurePrivateDir(dir)
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// safeFileName keeps only [A-Za-z0-9._-] from the browser-supplied name so the
// result can be pasted into any shell unquoted and never contains a path.
func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var builder strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '.', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	clean := strings.TrimLeft(builder.String(), ".")
	if strings.Trim(clean, "_") == "" {
		clean = "file"
	}
	if len(clean) > 80 {
		extension := filepath.Ext(clean)
		if len(extension) > 16 {
			extension = ""
		}
		clean = clean[:80-len(extension)] + extension
	}
	return clean
}

func (t *transfers) expire(now time.Time) {
	for id, item := range t.uploads {
		if now.Sub(item.touched) > transferIdleTime {
			_ = item.file.Close()
			_ = os.Remove(item.partPath)
			delete(t.uploads, id)
		}
	}
	for id, item := range t.exports {
		if now.Sub(item.touched) > transferIdleTime {
			delete(t.exports, id)
		}
	}
}

func (t *transfers) beginUpload(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	if request.Size <= 0 || request.Size > protocol.MaxUploadSize {
		return nil, transferFailure("too_large", "file must be between 1 byte and 20 MiB")
	}
	dir, err := uploadDir()
	if err != nil {
		return nil, transferFailure("upload_unavailable", "cannot prepare upload directory: "+err.Error())
	}
	id, err := transferID()
	if err != nil {
		return nil, err
	}
	name := time.Now().Format("20060102-150405") + "-" + id[:6] + "-" + safeFileName(request.FileName)
	partPath := filepath.Join(dir, "."+name+".part")
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(time.Now())
	if len(t.uploads) >= maxUploads {
		return nil, transferFailure("busy", "too many uploads in progress")
	}
	file, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, transferFailure("upload_unavailable", "cannot create upload file: "+err.Error())
	}
	t.uploads[id] = &upload{file: file, partPath: partPath, finalPath: filepath.Join(dir, name), name: name, size: request.Size, written: map[int64]int{}, touched: time.Now()}
	return &protocol.TransferResult{ID: id, FileName: name}, nil
}

func (t *transfers) lookupUpload(id string) (*upload, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	item := t.uploads[id]
	if item == nil {
		return nil, transferFailure("transfer_not_found", "upload not found or expired")
	}
	item.touched = time.Now()
	return item, nil
}

func (t *transfers) uploadChunk(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	if len(request.Data) == 0 || len(request.Data) > protocol.TransferChunkSize || request.Offset < 0 {
		return nil, transferFailure("invalid_request", "invalid upload chunk")
	}
	item, err := t.lookupUpload(request.ID)
	if err != nil {
		return nil, err
	}
	if request.Offset+int64(len(request.Data)) > item.size {
		return nil, transferFailure("too_large", "chunk exceeds declared size")
	}
	if _, err := item.file.WriteAt(request.Data, request.Offset); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if previous, seen := item.written[request.Offset]; !seen {
		item.received += int64(len(request.Data))
	} else if previous != len(request.Data) {
		item.received += int64(len(request.Data) - previous)
	}
	item.written[request.Offset] = len(request.Data)
	t.mu.Unlock()
	return &protocol.TransferResult{ID: request.ID}, nil
}

func (t *transfers) commitUpload(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	item, err := t.lookupUpload(request.ID)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	delete(t.uploads, request.ID)
	received := item.received
	t.mu.Unlock()
	fail := func(err error) (*protocol.TransferResult, error) {
		_ = item.file.Close()
		_ = os.Remove(item.partPath)
		return nil, err
	}
	if request.Size != item.size || received != item.size {
		return fail(transferFailure("checksum_mismatch", "upload is incomplete"))
	}
	if err := item.file.Sync(); err != nil {
		return fail(err)
	}
	hash := sha256.New()
	if _, err := item.file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	written, err := io.Copy(hash, item.file)
	if err != nil {
		return fail(err)
	}
	if written != item.size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), request.SHA256) {
		return fail(transferFailure("checksum_mismatch", "uploaded file checksum does not match"))
	}
	if err := item.file.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(item.partPath, item.finalPath); err != nil {
		_ = os.Remove(item.partPath)
		return nil, err
	}
	return &protocol.TransferResult{ID: request.ID, Path: item.finalPath, FileName: item.name, Size: item.size}, nil
}

func (t *transfers) abortUpload(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	t.mu.Lock()
	item := t.uploads[request.ID]
	delete(t.uploads, request.ID)
	t.mu.Unlock()
	if item != nil {
		_ = item.file.Close()
		_ = os.Remove(item.partPath)
	}
	return &protocol.TransferResult{ID: request.ID}, nil
}

// cleanUploads removes stale partial uploads and week-old files at start and
// every six hours, so pasted screenshots do not fill the disk. Idle transfers
// are released every transferIdleTime, because a browser that disconnects
// mid-transfer never sends abort or close.
func cleanUploads(logger *slog.Logger) {
	var lastPrune time.Time
	for {
		now := time.Now()
		activeTransfers.mu.Lock()
		activeTransfers.expire(now)
		activeTransfers.mu.Unlock()
		if now.Sub(lastPrune) >= uploadCleanupEvery {
			lastPrune = now
			if dir, err := uploadDir(); err == nil {
				removed := pruneUploads(dir, now)
				if removed > 0 {
					logger.Info("removed old uploads", "count", removed)
				}
			}
		}
		time.Sleep(transferIdleTime)
	}
}

// uploadNamePattern matches names created by beginUpload, so pruning never
// touches other files in a custom SINTHMUX_UPLOAD_DIR.
var uploadNamePattern = regexp.MustCompile(`^\.?\d{8}-\d{6}-[0-9a-f]{6}-`)

func pruneUploads(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !uploadNamePattern.MatchString(entry.Name()) {
			continue
		}
		limit := uploadRetention
		if strings.HasSuffix(entry.Name(), ".part") {
			limit = partialRetention
		}
		if now.Sub(info.ModTime()) > limit && os.Remove(filepath.Join(dir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

// captureHistory returns the session's active pane as plain text: the last
// lines of history (or all of it) plus the visible screen, wrapped lines joined
// and trailing blanks removed.
func captureHistory(ctx context.Context, session string, lines int) ([]byte, error) {
	start := "-"
	if lines > 0 {
		start = "-" + strconv.Itoa(lines)
	}
	output, err := runTmuxBytes(ctx, 20*time.Second, "capture-pane", "-p", "-J", "-t", paneTarget(session), "-S", start, "-E", "-")
	if err != nil {
		return nil, err
	}
	rows := bytes.Split(output, []byte("\n"))
	for index, row := range rows {
		rows[index] = bytes.TrimRight(row, " \t\r")
	}
	for len(rows) > 0 && len(rows[len(rows)-1]) == 0 {
		rows = rows[:len(rows)-1]
	}
	text := bytes.Join(rows, []byte("\n"))
	if len(text) > 0 {
		text = append(text, '\n')
	}
	return text, nil
}

func (t *transfers) beginExport(ctx context.Context, session string, request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	if !extendedTmux {
		return nil, transferFailure("unsupported_method", "terminal export is not supported on this device")
	}
	if request.Lines < 0 {
		return nil, transferFailure("invalid_request", "invalid line count")
	}
	t.mu.Lock()
	t.expire(time.Now())
	busy := len(t.exports) >= maxExports
	t.mu.Unlock()
	if busy {
		return nil, transferFailure("busy", "too many exports in progress")
	}
	data, err := captureHistory(ctx, session, request.Lines)
	if err != nil {
		return nil, err
	}
	if len(data) > maxExportSize {
		return nil, transferFailure("too_large", "terminal history exceeds 32 MiB; export fewer lines")
	}
	id, err := transferID()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.exports) >= maxExports {
		return nil, transferFailure("busy", "too many exports in progress")
	}
	t.exports[id] = &export{data: data, touched: time.Now()}
	return &protocol.TransferResult{ID: id, Size: int64(len(data))}, nil
}

func (t *transfers) readExport(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	t.mu.Lock()
	item := t.exports[request.ID]
	if item != nil {
		item.touched = time.Now()
	}
	t.mu.Unlock()
	if item == nil {
		return nil, transferFailure("transfer_not_found", "export not found or expired")
	}
	if request.Offset < 0 || request.Offset > int64(len(item.data)) {
		return nil, transferFailure("invalid_request", "invalid export offset")
	}
	end := min(request.Offset+protocol.TransferChunkSize, int64(len(item.data)))
	return &protocol.TransferResult{ID: request.ID, Data: item.data[request.Offset:end], EOF: end == int64(len(item.data))}, nil
}

func (t *transfers) closeExport(request *protocol.TransferRequest) (*protocol.TransferResult, error) {
	t.mu.Lock()
	delete(t.exports, request.ID)
	t.mu.Unlock()
	return &protocol.TransferResult{ID: request.ID}, nil
}

// handleTransfer serves file.upload.* and terminal.export.* RPCs.
func handleTransfer(ctx context.Context, request protocol.RPCRequest) (*protocol.TransferResult, error) {
	transfer := request.Transfer
	if transfer == nil {
		return nil, transferFailure("invalid_request", "missing transfer arguments")
	}
	switch request.Method {
	case "file.upload.begin":
		return activeTransfers.beginUpload(transfer)
	case "file.upload.chunk":
		return activeTransfers.uploadChunk(transfer)
	case "file.upload.commit":
		return activeTransfers.commitUpload(transfer)
	case "file.upload.abort":
		return activeTransfers.abortUpload(transfer)
	case "terminal.export.begin":
		if err := checkNames(request.Name); err != nil {
			return nil, err
		}
		return activeTransfers.beginExport(ctx, request.Name, transfer)
	case "terminal.export.read":
		return activeTransfers.readExport(transfer)
	case "terminal.export.close":
		return activeTransfers.closeExport(transfer)
	}
	return nil, transferFailure("unsupported_method", "unsupported RPC method")
}

var _ error = (*transferError)(nil)
