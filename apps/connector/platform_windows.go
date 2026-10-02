//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// psmux may leave format variables it does not implement unexpanded; a
// session list with zero counts is better than none.
const lenientSessionFields = true

// bundledTmuxName is psmux's tmux.exe, placed next to the connector by the installer.
const bundledTmuxName = "tmux.exe"

// psmux resolves -t by exact session name and does not strip tmux's "=" prefix
// on every command. Session names are already restricted to [A-Za-z0-9_-].
func sessionTarget(name string) string { return name }

// The connector runs without a console; keep tmux helper calls from flashing windows.
func hideConsole(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// The Windows connector is built as a GUI program so it never opens a console
// window; its stdout goes nowhere, so the log is written next to the config.
func defaultLogPath() string {
	root, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(root, "sinthmux", "connector.log")
}
