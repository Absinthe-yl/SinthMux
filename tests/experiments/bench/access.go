package main

import (
	"flag"
	"fmt"
	"time"
)

// runAccess opens a terminal N times per system and profile and records whether
// it reached the first screen within the web page's deadlines (8 s ticket, 10 s
// handshake, 10 s first output) and how long it took.
func runAccess(s Stack, flags *flag.FlagSet, args []string) error {
	n := flags.Int("n", 200, "attempts per system and profile")
	profileNames := flags.String("profiles", "lan,wan", "network profiles")
	session := flags.String("session", "exp-access", "tmux session")
	_ = flags.Parse(args)

	owner, err := login(s.Proxy, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	device, err := owner.deviceByName(s.Device, 20*time.Second)
	if err != nil {
		return err
	}
	if err := owner.ensureSession(device.ID, *session); err != nil {
		return err
	}
	sshCfg, err := sshConfig(s.SSHKey)
	if err != nil {
		return err
	}
	command := fmt.Sprintf("tmux -L %s -u attach-session -t =%s", s.Sock, *session)

	out, err := newTable(s, "access.csv", "system", "profile", "trial", "ok", "error", "stage1_ms", "stage2_ms", "first_screen_ms")
	if err != nil {
		return err
	}
	defer out.close()
	for _, p := range selectProfiles(*profileNames) {
		if err := applyProfile(s, p); err != nil {
			return err
		}
		time.Sleep(time.Second)
		for i := 0; i < *n; i++ {
			// Alternate systems so slow drift affects both alike.
			t, timing, err := openTerminal(owner, device.ID, *session, 10*time.Second)
			if err == nil {
				t.close()
				out.row("sinthmux", p.Name, i, 1, "", timing.ticket, timing.handshake, timing.first)
			} else {
				out.row("sinthmux", p.Name, i, 0, err.Error(), timing.ticket, timing.handshake, "")
			}
			st, sTiming, err := openSSH(s.SSH, sshCfg, command, 10*time.Second)
			if err == nil {
				st.close()
				out.row("ssh", p.Name, i, 1, "", sTiming.tcp, sTiming.handshake, sTiming.first)
			} else {
				out.row("ssh", p.Name, i, 0, err.Error(), sTiming.tcp, sTiming.handshake, "")
			}
			// tmux needs a moment to drop detached clients; a steady pace also
			// keeps the 511-pty limit far away.
			time.Sleep(150 * time.Millisecond)
			if (i+1)%50 == 0 {
				logf("access %s %d/%d", p.Name, i+1, *n)
			}
		}
	}
	return applyProfile(s, profiles["lan"])
}

// keys avoids letters that appear in the escape sequences tmux sends for line
// editing (h, l, m, r, j, J, K, H …), so a byte match means the echo arrived.
const keys = "abcdefgknopqstuvwxyz"

// runLatency types single characters at a bash prompt and measures the time
// until the echoed character comes back, for SinthMux and SSH on the same tmux
// session. Every 20 keys the line is cleared with Ctrl-U.
func runLatency(s Stack, flags *flag.FlagSet, args []string) error {
	n := flags.Int("n", 500, "keystrokes per system and profile")
	gap := flags.Duration("gap", 120*time.Millisecond, "pause between keystrokes")
	profileNames := flags.String("profiles", "lan,wan", "network profiles")
	session := flags.String("session", "exp-latency", "tmux session")
	_ = flags.Parse(args)

	owner, err := login(s.Proxy, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	device, err := owner.deviceByName(s.Device, 20*time.Second)
	if err != nil {
		return err
	}
	if err := owner.ensureSession(device.ID, *session); err != nil {
		return err
	}
	sshCfg, err := sshConfig(s.SSHKey)
	if err != nil {
		return err
	}
	command := fmt.Sprintf("tmux -L %s -u attach-session -t =%s", s.Sock, *session)
	out, err := newTable(s, "latency.csv", "system", "profile", "key", "ok", "latency_ms")
	if err != nil {
		return err
	}
	defer out.close()

	type typer struct {
		send   func([]byte) error
		frames <-chan frame
		done   <-chan struct{}
		close  func()
	}
	measure := func(system string, p Profile, t typer) {
		defer t.close()
		drainFrames(t.frames, 800*time.Millisecond, 5*time.Second)
		_ = t.send([]byte{0x15})
		drainFrames(t.frames, 300*time.Millisecond, 3*time.Second)
		for i := 0; i < *n; i++ {
			key := keys[i%len(keys)]
			start := time.Now()
			if err := t.send([]byte{key}); err != nil {
				out.row(system, p.Name, i, 0, "")
				continue
			}
			at, ok := waitByte(t.frames, t.done, key, 3*time.Second)
			if ok {
				out.row(system, p.Name, i, 1, at.Sub(start))
			} else {
				out.row(system, p.Name, i, 0, "")
			}
			drainFrames(t.frames, 30*time.Millisecond, 200*time.Millisecond)
			if (i+1)%20 == 0 {
				_ = t.send([]byte{0x15})
				drainFrames(t.frames, 150*time.Millisecond, 2*time.Second)
			}
			time.Sleep(*gap)
		}
	}
	for _, p := range selectProfiles(*profileNames) {
		if err := applyProfile(s, p); err != nil {
			return err
		}
		time.Sleep(time.Second)
		t, _, err := openTerminal(owner, device.ID, *session, 10*time.Second)
		if err != nil {
			return fmt.Errorf("sinthmux terminal: %w", err)
		}
		measure("sinthmux", p, typer{send: t.send, frames: t.frames, done: t.done, close: t.close})
		logf("latency sinthmux %s done", p.Name)
		st, _, err := openSSH(s.SSH, sshCfg, command, 10*time.Second)
		if err != nil {
			return fmt.Errorf("ssh terminal: %w", err)
		}
		measure("ssh", p, typer{send: st.send, frames: st.frames, done: st.done, close: st.close})
		logf("latency ssh %s done", p.Name)
	}
	return applyProfile(s, profiles["lan"])
}
