package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestTmuxSessionLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socketDir, err := os.MkdirTemp("/tmp", "sinthmux-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	ctx := context.Background()
	call := func(request protocol.RPCRequest) *protocol.RPCResponse {
		t.Helper()
		response := handleRPC(ctx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: "test", Request: &request}).Response
		if response == nil {
			t.Fatal("missing RPC response")
		}
		return response
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.list"}); !response.OK || len(response.Sessions) != 0 {
		t.Fatalf("list before tmux server starts: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.create", Name: "sinthmux_test"}); !response.OK {
		t.Fatalf("create: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.create", Name: "sinthmux_test"}); response.ErrorCode != "already_exists" {
		t.Fatalf("duplicate: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.list"}); !response.OK || len(response.Sessions) != 1 || response.Sessions[0].Name != "sinthmux_test" {
		t.Fatalf("list: %+v", response)
	}
	if output, err := exec.Command("tmux", "new-session", "-d", "-s", "external|session").CombinedOutput(); err != nil {
		t.Fatalf("create external session: %v: %s", err, output)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.list"}); !response.OK || len(response.Sessions) != 2 {
		t.Fatalf("list with external session: %+v", response)
	} else if response.Sessions[0].Name != "external|session" && response.Sessions[1].Name != "external|session" {
		t.Fatalf("external session name was not preserved: %+v", response.Sessions)
	}
	if output, err := exec.Command("tmux", "kill-session", "-t", "=external|session").CombinedOutput(); err != nil {
		t.Fatalf("close external session: %v: %s", err, output)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.rename", Name: "sinthmux_test", NewName: "renamed"}); !response.OK {
		t.Fatalf("rename: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.close", Name: "sinthmux_test"}); response.ErrorCode != "not_found" {
		t.Fatalf("old name should not match: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.close", Name: "renamed"}); !response.OK {
		t.Fatalf("close: %+v", response)
	}
	if response := call(protocol.RPCRequest{Method: "tmux.sessions.list"}); !response.OK || len(response.Sessions) != 0 {
		t.Fatalf("empty list: %+v", response)
	}
}

func TestTmuxAbsolutePathWorksWithoutServicePATH(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	socketDir, err := os.MkdirTemp("/tmp", "sm-path-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SINTHMUX_TMUX_BIN", binary)
	t.Cleanup(func() { _ = exec.Command(binary, "kill-server").Run() })

	if err := createSession(context.Background(), "path_test"); err != nil {
		t.Fatalf("create with restricted PATH: %v", err)
	}
	sessions, err := listSessions(context.Background())
	if err != nil || len(sessions) != 1 || sessions[0].Name != "path_test" {
		t.Fatalf("list with restricted PATH: %+v, %v", sessions, err)
	}
	t.Setenv("SINTHMUX_TMUX_BIN", filepath.Join(socketDir, "missing-tmux"))
	if _, err := listSessions(context.Background()); err == nil {
		t.Fatal("missing tmux path was accepted")
	}
}

func TestInvalidSessionNames(t *testing.T) {
	for _, name := range []string{"", "-bad", "a:b", "a.b", "a b", "a;echo", "名字"} {
		response := handleRPC(context.Background(), protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: "test", Request: &protocol.RPCRequest{Method: "tmux.sessions.create", Name: name}}).Response
		if response == nil || response.ErrorCode != "invalid_name" {
			t.Fatalf("name %q: %+v", name, response)
		}
	}
}
