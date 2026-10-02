//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// windowsTerminal runs the tmux-compatible client (psmux) inside a ConPTY.
type windowsTerminal struct {
	*conpty.ConPty
	stop context.CancelFunc
	once sync.Once
	done chan struct{}
}

func (t *windowsTerminal) Resize(cols, rows uint16) error {
	return t.ConPty.Resize(int(cols), int(rows))
}

func (t *windowsTerminal) Wait() error {
	<-t.done
	return nil
}

func (t *windowsTerminal) Close() error {
	var err error
	t.once.Do(func() {
		t.stop()
		err = t.ConPty.Close()
	})
	return err
}

func startAttach(ctx context.Context, session string, cols, rows uint16) (terminalProcess, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, errors.New("this Windows version has no ConPTY; Windows 10 1809 or newer is required")
	}
	home, _ := os.UserHomeDir()
	parts := []string{tmuxExecutable()}
	parts = append(parts, tmuxArgs("attach-session", "-t", sessionTarget(session))...)
	for i, part := range parts {
		parts[i] = windows.EscapeArg(part)
	}
	command := strings.Join(parts, " ")
	env := append(os.Environ(), "TERM=xterm-256color")
	cpty, err := conpty.Start(command, conpty.ConPtyDimensions(int(cols), int(rows)), conpty.ConPtyWorkDir(home), conpty.ConPtyEnv(env))
	if err != nil {
		return nil, err
	}
	waitCtx, stop := context.WithCancel(ctx)
	terminal := &windowsTerminal{ConPty: cpty, stop: stop, done: make(chan struct{})}
	go func() {
		_, _ = cpty.Wait(waitCtx)
		close(terminal.done)
		// Closing the pseudo console ends the pending Read with an error, the
		// same way a Unix PTY reports EOF when its child exits.
		_ = terminal.Close()
	}()
	return terminal, nil
}
