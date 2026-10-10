package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// isolatedTmux points tmux at a private socket directory for one test.
func isolatedTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socketDir, err := os.MkdirTemp("/tmp", "sinthmux-tier1-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	t.Setenv("SINTHMUX_TMUX_SOCKET", "")
	t.Setenv("SINTHMUX_TMUX_BIN", "")
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(socketDir)
	})
}

func rpc(t *testing.T, method, name string, transfer *protocol.TransferRequest) *protocol.RPCResponse {
	t.Helper()
	request := protocol.RPCRequest{Method: method, Name: name, Transfer: transfer}
	return handleRPC(context.Background(), protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: "t", Request: &request}).Response
}

func TestSafeFileName(t *testing.T) {
	for input, want := range map[string]string{
		"screenshot.png":          "screenshot.png",
		"../../etc/passwd":        "passwd",
		`C:\Users\me\photo 1.JPG`: "photo_1.JPG",
		"中文 a b.png":              "___a_b.png",
		".hidden":                 "hidden",
		"":                        "file",
		"///":                     "file",
		"a;rm -rf ~.txt":          "a_rm_-rf__.txt",
	} {
		if got := safeFileName(input); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", input, got, want)
		}
	}
	long := safeFileName(strings.Repeat("x", 300) + ".tar.gz")
	if len(long) != 80 || !strings.HasSuffix(long, ".gz") {
		t.Errorf("long name: %q (%d)", long, len(long))
	}
}

func TestUploadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SINTHMUX_UPLOAD_DIR", filepath.Join(dir, "uploads"))
	data := make([]byte, 3*protocol.TransferChunkSize+17)
	_, _ = rand.Read(data)
	sum := sha256.Sum256(data)

	begin := rpc(t, "file.upload.begin", "", &protocol.TransferRequest{FileName: "../../a b.png", Size: int64(len(data))})
	if !begin.OK {
		t.Fatalf("begin: %+v", begin)
	}
	id := begin.Transfer.ID
	// Chunks may arrive in any order.
	for _, offset := range []int64{2 * protocol.TransferChunkSize, 0, 3 * protocol.TransferChunkSize, protocol.TransferChunkSize} {
		end := min(offset+protocol.TransferChunkSize, int64(len(data)))
		if response := rpc(t, "file.upload.chunk", "", &protocol.TransferRequest{ID: id, Offset: offset, Data: data[offset:end]}); !response.OK {
			t.Fatalf("chunk %d: %+v", offset, response)
		}
	}
	commit := rpc(t, "file.upload.commit", "", &protocol.TransferRequest{ID: id, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	if !commit.OK {
		t.Fatalf("commit: %+v", commit)
	}
	path := commit.Transfer.Path
	if filepath.Dir(path) != filepath.Join(dir, "uploads") || !strings.HasSuffix(path, "-a_b.png") || strings.ContainsAny(filepath.Base(path), " /") {
		t.Fatalf("path: %s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %v", err)
	}
	info, _ := os.Stat(path)
	dirInfo, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("modes file=%v dir=%v", info.Mode().Perm(), dirInfo.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestUploadFailuresLeaveNoPartialFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	t.Setenv("SINTHMUX_UPLOAD_DIR", dir)
	if response := rpc(t, "file.upload.begin", "", &protocol.TransferRequest{FileName: "a", Size: protocol.MaxUploadSize + 1}); response.OK || response.ErrorCode != "too_large" {
		t.Fatalf("oversized begin: %+v", response)
	}
	begin := rpc(t, "file.upload.begin", "", &protocol.TransferRequest{FileName: "a", Size: 4})
	id := begin.Transfer.ID
	if response := rpc(t, "file.upload.chunk", "", &protocol.TransferRequest{ID: id, Offset: 2, Data: []byte("abc")}); response.OK || response.ErrorCode != "too_large" {
		t.Fatalf("overflow chunk: %+v", response)
	}
	rpc(t, "file.upload.chunk", "", &protocol.TransferRequest{ID: id, Offset: 0, Data: []byte("abcd")})
	if response := rpc(t, "file.upload.commit", "", &protocol.TransferRequest{ID: id, Size: 4, SHA256: strings.Repeat("0", 64)}); response.OK || response.ErrorCode != "checksum_mismatch" {
		t.Fatalf("bad checksum: %+v", response)
	}
	second := rpc(t, "file.upload.begin", "", &protocol.TransferRequest{FileName: "b", Size: 4}).Transfer.ID
	rpc(t, "file.upload.abort", "", &protocol.TransferRequest{ID: second})
	if response := rpc(t, "file.upload.chunk", "", &protocol.TransferRequest{ID: second, Data: []byte("x")}); response.ErrorCode != "transfer_not_found" {
		t.Fatalf("chunk after abort: %+v", response)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestUploadDirRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "uploads")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SINTHMUX_UPLOAD_DIR", filepath.Join(dir, "uploads"))
	if response := rpc(t, "file.upload.begin", "", &protocol.TransferRequest{FileName: "a", Size: 1}); response.OK || response.ErrorCode != "upload_unavailable" {
		t.Fatalf("symlinked dir accepted: %+v", response)
	}
}

func TestPruneUploads(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old, fresh, oldPart, freshPart := "20260101-120000-abcdef-old.png", "20260101-120000-abcdef-new.png", ".20260101-120000-abcdef-x.part", ".20260101-120000-abcdef-y.part"
	for name, age := range map[string]time.Duration{old: 8 * 24 * time.Hour, fresh: time.Hour, oldPart: 2 * time.Hour, freshPart: time.Minute, "notes.txt": 30 * 24 * time.Hour} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte("x"), 0o600)
		_ = os.Chtimes(path, now.Add(-age), now.Add(-age))
	}
	if removed := pruneUploads(dir, now); removed != 2 {
		t.Fatalf("removed %d", removed)
	}
	for _, name := range []string{fresh, freshPart, "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s removed", name)
		}
	}
}

func tmuxRun(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v %s", args, err, output)
	}
	return string(output)
}

func waitForPane(t *testing.T, session, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(tmuxRun(t, "capture-pane", "-p", "-t", "="+session+":"), text) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s never showed %q", session, text)
}

func readExport(t *testing.T, session string, lines int) []byte {
	t.Helper()
	begin := rpc(t, "terminal.export.begin", session, &protocol.TransferRequest{Lines: lines})
	if !begin.OK {
		t.Fatalf("export begin: %+v", begin)
	}
	var result []byte
	for offset := int64(0); ; {
		read := rpc(t, "terminal.export.read", "", &protocol.TransferRequest{ID: begin.Transfer.ID, Offset: offset})
		if !read.OK {
			t.Fatalf("export read: %+v", read)
		}
		result = append(result, read.Transfer.Data...)
		offset += int64(len(read.Transfer.Data))
		if read.Transfer.EOF {
			break
		}
	}
	rpc(t, "terminal.export.close", "", &protocol.TransferRequest{ID: begin.Transfer.ID})
	if int64(len(result)) != begin.Transfer.Size {
		t.Fatalf("export size %d, announced %d", len(result), begin.Transfer.Size)
	}
	return result
}

func TestExportCapturesHistory(t *testing.T) {
	if !extendedTmux {
		t.Skip("export is not offered on this platform")
	}
	isolatedTmux(t)
	tmuxRun(t, "new-session", "-d", "-s", "exp", "-x", "120", "-y", "30")
	tmuxRun(t, "new-session", "-d", "-s", "exp2")
	tmuxRun(t, "send-keys", "-t", "=exp:", "for i in $(seq 1 1500); do echo \"L$i 中文🙂\"; done; echo DONE-$((40+2))", "Enter")
	// The marker is computed so the echoed command line cannot match it.
	waitForPane(t, "exp", "DONE-42")

	all := string(readExport(t, "exp", 0))
	for _, want := range []string{"L1 中文🙂\n", "L750 中文🙂\n", "L1500 中文🙂\n"} {
		if !strings.Contains(all, want) {
			t.Fatalf("full export misses %q", want)
		}
	}
	if strings.Index(all, "L1 中文") > strings.Index(all, "L1500 中文") || strings.HasSuffix(all, "\n\n") || strings.Contains(all, " \n") {
		t.Fatal("export out of order or has trailing blanks")
	}
	recent := string(readExport(t, "exp", 100))
	if strings.Contains(recent, "L1 中文") || !strings.Contains(recent, "L1500 中文") {
		t.Fatal("lines=100 export has the wrong range")
	}
	if strings.Contains(string(readExport(t, "exp2", 0)), "L1500") {
		t.Fatal("exp2 export read the wrong session")
	}
	if response := rpc(t, "terminal.export.begin", "missing", &protocol.TransferRequest{}); response.OK || response.ErrorCode != "not_found" {
		t.Fatalf("missing session: %+v", response)
	}
	if response := rpc(t, "terminal.export.read", "", &protocol.TransferRequest{ID: "nope"}); response.ErrorCode != "transfer_not_found" {
		t.Fatalf("unknown export: %+v", response)
	}
}

func TestNotificationEncoding(t *testing.T) {
	value := encodeNotification("green", "构建完成 #1 | ok", time.UnixMilli(1700000000123))
	if strings.ContainsAny(value, "|#") {
		t.Fatalf("value leaks tmux format characters: %s", value)
	}
	items := parseNotifications("odd|name|" + value + "\nidle|\nwork|" + value + "\nbad|v1:x\n")
	if len(items) != 1 || items[0] != (protocol.SessionNotification{Session: "work", Color: "green", Message: "构建完成 #1 | ok", At: 1700000000123}) {
		t.Fatalf("parsed: %+v", items)
	}
	if got := truncateUTF8(strings.Repeat("中", 100), protocol.MaxNotificationMessage); len(got) > protocol.MaxNotificationMessage || !strings.HasPrefix(strings.Repeat("中", 100), got) {
		t.Fatalf("truncate: %d", len(got))
	}
	if item, ok := decodeNotification("s", "v1:1:purple:"); !ok || item.Color != "blue" {
		t.Fatalf("unknown color: %+v", item)
	}
}

func TestNotifyCommandAndWatcher(t *testing.T) {
	if !notifySupported {
		t.Skip("notifications are not offered on this platform")
	}
	isolatedTmux(t)
	binary := filepath.Join(t.TempDir(), "sinthmux-connector")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	tmuxRun(t, "new-session", "-d", "-s", "work")
	tmuxRun(t, "new-session", "-d", "-s", "work2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher := newNotifier(slog.New(slog.NewTextHandler(io.Discard, nil)))
	go watcher.watch(ctx)
	snapshots := make(chan protocol.NotificationSnapshot, 16)
	go watcher.publish(ctx, func(snapshot protocol.NotificationSnapshot) error { snapshots <- snapshot; return nil })
	next := func(want func([]protocol.SessionNotification) bool) []protocol.SessionNotification {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case snapshot := <-snapshots:
				if want(snapshot.Items) {
					return snapshot.Items
				}
			case <-timeout:
				t.Fatal("no matching notification snapshot")
			}
		}
	}
	next(func(items []protocol.SessionNotification) bool { return len(items) == 0 })

	start := time.Now()
	tmuxRun(t, "send-keys", "-t", "=work:", fmt.Sprintf("%q notify --color red 'e2e 完成'", binary), "Enter")
	items := next(func(items []protocol.SessionNotification) bool { return len(items) == 1 })
	if items[0].Session != "work" || items[0].Color != "red" || items[0].Message != "e2e 完成" {
		t.Fatalf("notification: %+v", items)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("notification took %v; the wait-for wake-up did not work", elapsed)
	}
	// --session from outside tmux, then clear via RPC as the Web UI does.
	command := exec.Command(binary, "notify", "--session", "work2", "--color", "yellow", "second")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("notify --session: %v %s", err, output)
	}
	next(func(items []protocol.SessionNotification) bool { return len(items) == 2 })
	if response := rpc(t, "session.notify.clear", "work", nil); !response.OK {
		t.Fatalf("clear: %+v", response)
	}
	items = next(func(items []protocol.SessionNotification) bool { return len(items) == 1 })
	if items[0].Session != "work2" {
		t.Fatalf("after clear: %+v", items)
	}
	watcher.clear(ctx, "work2")
	next(func(items []protocol.SessionNotification) bool { return len(items) == 0 })

	outside := exec.Command(binary, "notify", "hello")
	outside.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=")
	if err := outside.Run(); err == nil || outside.ProcessState.ExitCode() != 2 {
		t.Fatalf("notify outside tmux: %v", err)
	}
}
