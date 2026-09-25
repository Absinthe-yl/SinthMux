package main

import (
	"context"
	"os"
	"os/exec"
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

func TestInvalidSessionNames(t *testing.T) {
	for _, name := range []string{"", "-bad", "a:b", "a.b", "a b", "a;echo", "名字"} {
		response := handleRPC(context.Background(), protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: "test", Request: &protocol.RPCRequest{Method: "tmux.sessions.create", Name: name}}).Response
		if response == nil || response.ErrorCode != "invalid_name" {
			t.Fatalf("name %q: %+v", name, response)
		}
	}
}
