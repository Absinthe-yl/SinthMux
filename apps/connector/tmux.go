package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	case "file.upload.begin", "file.upload.chunk", "file.upload.commit", "file.upload.abort", "terminal.export.begin", "terminal.export.read", "terminal.export.close":
		result.Response.Transfer, err = handleTransfer(ctx, *request)
	case "session.notify.clear":
		err = checkNames(request.Name)
		if err == nil {
			err = clearNotification(ctx, request.Name)
		}
	default:
		err = &tmuxError{code: "unsupported_method", message: "unsupported RPC method"}
	}
	if err != nil {
		result.Response.Name = ""
		result.Response.Sessions = nil
		result.Response.Transfer = nil
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

// tmuxExecutable prefers an explicit SINTHMUX_TMUX_BIN, then a tmux that the
// installer placed next to the connector (the Hub-provided bundle), then PATH.
func tmuxExecutable() string {
	if path := os.Getenv("SINTHMUX_TMUX_BIN"); filepath.IsAbs(path) {
		return path
	}
	if self, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(self), bundledTmuxName)
		if info, err := os.Stat(bundled); err == nil && !info.IsDir() {
			return bundled
		}
	}
	return "tmux"
}

// tmuxSocketPath, when set, pins tmux to one server socket (tmux -S). The
// notify command uses it to reach the server of the pane it runs in.
var tmuxSocketPath string

// tmuxArgs selects the tmux server. SINTHMUX_TMUX_SOCKET (tmux -L) is set by
// the installer when another tmux version already owns the default socket,
// because tmux clients cannot talk to a server of a different protocol.
func tmuxArgs(args ...string) []string {
	if tmuxSocketPath != "" {
		return append([]string{"-S", tmuxSocketPath}, args...)
	}
	if socket := os.Getenv("SINTHMUX_TMUX_SOCKET"); protocol.ValidSessionName(socket) {
		return append([]string{"-L", socket}, args...)
	}
	return args
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	output, err := runTmuxBytes(ctx, 5*time.Second, args...)
	return string(output), err
}

// runTmuxBytes runs tmux with a timeout and returns stdout. stderr is kept
// apart so captured terminal text is never mixed with tmux warnings.
func runTmuxBytes(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, tmuxExecutable(), tmuxArgs(args...)...)
	hideConsole(command)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	if commandCtx.Err() != nil {
		return nil, &tmuxError{code: "timeout", message: "tmux command timed out"}
	}
	if errors.Is(err, exec.ErrNotFound) || os.IsNotExist(err) {
		return nil, &tmuxError{code: "tmux_unavailable", message: "设备代理找不到 tmux；请在设备上安装 tmux 后重新运行接入命令"}
	}
	return nil, classifyTmuxError(strings.TrimSpace(stderr.String() + "\n" + string(output)))
}

func classifyTmuxError(message string) error {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "duplicate session"):
		return &tmuxError{code: "already_exists", message: "session already exists"}
	case strings.Contains(lower, "can't find session"), strings.Contains(lower, "can't find pane"), strings.Contains(lower, "no such session"), strings.Contains(lower, "no server running"), strings.Contains(lower, "failed to connect to server"), strings.Contains(lower, "error connecting to") && strings.Contains(lower, "no such file or directory"):
		return &tmuxError{code: "not_found", message: "session not found"}
	default:
		return &tmuxError{code: "tmux_error", message: fmt.Sprintf("tmux command failed: %s", message)}
	}
}

func listSessions(ctx context.Context) ([]protocol.TmuxSession, error) {
	output, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}|#{session_windows}|#{session_attached}|#{session_created}")
	if err != nil {
		var commandErr *tmuxError
		if errors.As(err, &commandErr) && commandErr.code == "not_found" {
			return []protocol.TmuxSession{}, nil
		}
		return nil, err
	}
	sessions := make([]protocol.TmuxSession, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			return nil, &tmuxError{code: "invalid_output", message: "invalid tmux session output"}
		}
		name := strings.Join(fields[:len(fields)-3], "|")
		windows, windowsErr := strconv.Atoi(fields[len(fields)-3])
		attached, attachedErr := strconv.Atoi(fields[len(fields)-2])
		createdAt, createdErr := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if (windowsErr != nil || attachedErr != nil || createdErr != nil) && !lenientSessionFields {
			return nil, &tmuxError{code: "invalid_output", message: "invalid tmux session output"}
		}
		sessions = append(sessions, protocol.TmuxSession{Name: name, Windows: windows, Attached: attached > 0, CreatedAt: createdAt})
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
	_, err := runTmux(ctx, "rename-session", "-t", sessionTarget(name), newName)
	return err
}

func closeSession(ctx context.Context, name string) error {
	_, err := runTmux(ctx, "kill-session", "-t", sessionTarget(name))
	return err
}
