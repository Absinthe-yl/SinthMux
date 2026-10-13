package main

import (
	"errors"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshTerminal is the SSH baseline: ssh → pty → tmux attach on the same device.
type sshTerminal struct {
	client  *ssh.Client
	session *ssh.Session
	stdin   interface{ Write([]byte) (int, error) }
	frames  chan frame
	done    chan struct{}
}

type sshTiming struct {
	tcp, handshake, first time.Duration
}

func sshConfig(keyPath string) (*ssh.ClientConfig, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{User: "bench", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}, nil
}

// openSSH connects and attaches to the tmux session, like `ssh -t host tmux attach -t =name`.
func openSSH(address string, config *ssh.ClientConfig, command string, timeout time.Duration) (*sshTerminal, sshTiming, error) {
	var timing sshTiming
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return nil, timing, err
	}
	timing.tcp = time.Since(start)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	c, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		conn.Close()
		return nil, timing, err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(c, channels, requests)
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, timing, err
	}
	if err := session.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		client.Close()
		return nil, timing, err
	}
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err := session.Start(command); err != nil {
		client.Close()
		return nil, timing, err
	}
	timing.handshake = time.Since(start) - timing.tcp
	t := &sshTerminal{client: client, session: session, stdin: stdin, frames: make(chan frame, 8192), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		buffer := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buffer)
			if n > 0 {
				select {
				case t.frames <- frame{at: time.Now(), data: append([]byte(nil), buffer[:n]...)}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case f := <-t.frames:
		timing.first = f.at.Sub(start)
	case <-t.done:
		t.close()
		return nil, timing, errors.New("ssh closed before output")
	case <-time.After(timeout):
		t.close()
		return nil, timing, errors.New("no first output")
	}
	return t, timing, nil
}

func (t *sshTerminal) send(data []byte) error { _, err := t.stdin.Write(data); return err }

func (t *sshTerminal) close() { _ = t.client.Close() }

// keepalive sends keepalive@openssh.com every interval and closes the client
// after countMax unanswered probes (ServerAliveInterval / ServerAliveCountMax).
func (t *sshTerminal) keepalive(interval time.Duration, countMax int) {
	go func() {
		missed := 0
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
			}
			reply := make(chan error, 1)
			go func() { _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); reply <- err }()
			select {
			case err := <-reply:
				if err != nil {
					t.close()
					return
				}
				missed = 0
			case <-time.After(interval):
				missed++
				if missed >= countMax {
					t.close()
					return
				}
			}
		}
	}()
}
