package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestTerminalAttachUsesUTF8(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socketDir, err := os.MkdirTemp("/tmp", "sinthmux-utf8-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	if output, err := exec.Command("tmux", "new-session", "-d", "-s", "utf8_test").CombinedOutput(); err != nil {
		t.Fatalf("create session: %v: %s", err, output)
	}

	streams := newTerminalStreams(func(protocol.Envelope) error { return nil })
	t.Cleanup(streams.closeAll)
	streams.open(&protocol.StreamOpen{StreamID: "test", Session: "utf8_test", Cols: 80, Rows: 24})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, err := exec.Command("tmux", "list-clients", "-F", "#{client_utf8}").CombinedOutput()
		if err == nil && strings.TrimSpace(string(output)) == "1" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("attached tmux client did not enable UTF-8")
}
