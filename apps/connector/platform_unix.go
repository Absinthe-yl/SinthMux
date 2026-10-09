//go:build !windows

package main

import "os/exec"

// lenientSessionFields is false because real tmux always prints numeric
// window, attached and created fields; anything else is a broken binary.
const lenientSessionFields = false

// bundledTmuxName is the tmux the installer may place next to the connector.
const bundledTmuxName = "tmux"

// sessionTarget uses tmux's "=" prefix so "dev" never matches "dev2".
func sessionTarget(name string) string { return "=" + name }

// paneTarget addresses the active pane of a session. Pane and option commands
// (capture-pane, set-option, send-keys) reject a bare "=name" and need "=name:".
func paneTarget(name string) string { return "=" + name + ":" }

// extendedTmux reports whether terminal export and session notifications are
// offered; they rely on capture-pane ranges, user options and wait-for.
const extendedTmux = true

func hideConsole(*exec.Cmd) {}

// defaultLogPath is empty: launchd, systemd and nohup already capture stdout.
func defaultLogPath() string { return "" }
