//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixTerminal struct {
	*os.File
	command *exec.Cmd
}

func (t *unixTerminal) Resize(cols, rows uint16) error {
	return pty.Setsize(t.File, &pty.Winsize{Cols: cols, Rows: rows})
}

func (t *unixTerminal) Wait() error { return t.command.Wait() }

func startAttach(ctx context.Context, session string, cols, rows uint16) (terminalProcess, error) {
	// launchd may provide a non-UTF-8 locale; tmux must send Unicode to the web terminal.
	command := exec.CommandContext(ctx, tmuxExecutable(), tmuxArgs("-u", "attach-session", "-t", "="+session)...)
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	file, err := pty.StartWithSize(command, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &unixTerminal{File: file, command: command}, nil
}
