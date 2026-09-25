package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type tmuxError struct {
	code    string
	message string
}

func (e *tmuxError) Error() string { return e.message }

func handleRPC(ctx context.Context, envelope protocol.Envelope) protocol.Envelope {
	result := protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCResponse, RequestID: envelope.RequestID, Response: &protocol.RPCResponse{}}
	if envelope.Version != protocol.Version || envelope.RequestID == "" || envelope.Request == nil {
		result.Response.ErrorCode = "invalid_request"
		result.Response.Error = "invalid RPC request"
		return result
	}

	request := envelope.Request
	var err error
	switch request.Method {
	case "tmux.sessions.list":
		result.Response.Sessions, err = listSessions(ctx)
	case "tmux.sessions.create":
		err = checkNames(request.Name)
		if err == nil {
			err = createSession(ctx, request.Name)
			result.Response.Name = request.Name
		}
	case "tmux.sessions.rename":
		err = checkNames(request.Name, request.NewName)
		if err == nil {
			err = renameSession(ctx, request.Name, request.NewName)
			result.Response.Name = request.NewName
		}
	case "tmux.sessions.close":
		err = checkNames(request.Name)
		if err == nil {
			err = closeSession(ctx, request.Name)
		}
	default:
		err = &tmuxError{code: "unsupported_method", message: "unsupported RPC method"}
	}
	if err != nil {
		result.Response.Name = ""
		result.Response.Sessions = nil
		result.Response.Error = err.Error()
		var commandErr *tmuxError
		if errors.As(err, &commandErr) {
			result.Response.ErrorCode = commandErr.code
		} else {
			result.Response.ErrorCode = "tmux_error"
		}
		return result
	}
	result.Response.OK = true
	return result
}

func checkNames(names ...string) error {
	for _, name := range names {
		if !protocol.ValidSessionName(name) {
			return &tmuxError{code: "invalid_name", message: "session name must start with a letter or digit and contain only letters, digits, underscores or hyphens (max 64 characters)"}
		}
	}
	return nil
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "tmux", args...).CombinedOutput()
	if err == nil {
		return string(output), nil
	}
	if commandCtx.Err() != nil {
		return "", &tmuxError{code: "timeout", message: "tmux command timed out"}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", &tmuxError{code: "tmux_unavailable", message: "tmux is not installed on this device"}
	}
	message := strings.TrimSpace(string(output))
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "duplicate session"):
		return "", &tmuxError{code: "already_exists", message: "session already exists"}
	case strings.Contains(lower, "can't find session"), strings.Contains(lower, "no such session"), strings.Contains(lower, "no server running"), strings.Contains(lower, "failed to connect to server"), strings.Contains(lower, "error connecting to") && strings.Contains(lower, "no such file or directory"):
		return "", &tmuxError{code: "not_found", message: "session not found"}
	default:
		return "", &tmuxError{code: "tmux_error", message: fmt.Sprintf("tmux command failed: %s", message)}
	}
}

func listSessions(ctx context.Context) ([]protocol.TmuxSession, error) {
	output, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}")
	if err != nil {
		var commandErr *tmuxError
		if errors.As(err, &commandErr) && commandErr.code == "not_found" {
			return []protocol.TmuxSession{}, nil
		}
		return nil, err
	}
	sessions := make([]protocol.TmuxSession, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, &tmuxError{code: "invalid_output", message: "invalid tmux session output"}
		}
		windows, windowsErr := strconv.Atoi(fields[1])
		attached, attachedErr := strconv.Atoi(fields[2])
		createdAt, createdErr := strconv.ParseInt(fields[3], 10, 64)
		if windowsErr != nil || attachedErr != nil || createdErr != nil {
			return nil, &tmuxError{code: "invalid_output", message: "invalid tmux session output"}
		}
		sessions = append(sessions, protocol.TmuxSession{Name: fields[0], Windows: windows, Attached: attached > 0, CreatedAt: createdAt})
	}
	return sessions, nil
}

func createSession(ctx context.Context, name string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_, err = runTmux(ctx, "new-session", "-d", "-s", name, "-c", home)
	return err
}

func renameSession(ctx context.Context, name, newName string) error {
	_, err := runTmux(ctx, "rename-session", "-t", "="+name, newName)
	return err
}

func closeSession(ctx context.Context, name string) error {
	_, err := runTmux(ctx, "kill-session", "-t", "="+name)
	return err
}
