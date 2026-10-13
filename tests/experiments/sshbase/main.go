// Command sshbase is the SSH baseline for tests/experiments: a minimal SSH
// server (golang.org/x/crypto/ssh) that accepts one public key, allocates a
// pty and runs the requested command, e.g. "tmux -L sock attach -t =s". It
// stands in for OpenSSH because macOS sshd cannot run unprivileged
// (sandbox_init is denied) and Remote Login is off on the test machine. The
// data path is the same as with OpenSSH: TCP → SSH transport → pty → tmux client.
//
//	sshbase -listen 127.0.0.1:18096 -hostkey hk -authorized ck.pub
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18096", "listen address")
	hostKey := flag.String("hostkey", "", "host private key (OpenSSH format)")
	authorized := flag.String("authorized", "", "the one accepted public key (authorized_keys format)")
	flag.Parse()

	keyData, err := os.ReadFile(*hostKey)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		log.Fatal(err)
	}
	authData, err := os.ReadFile(*authorized)
	if err != nil {
		log.Fatal(err)
	}
	allowed, _, _, _, err := ssh.ParseAuthorizedKey(authData)
	if err != nil {
		log.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), allowed.Marshal()) {
			return nil, nil
		}
		return nil, ssh.ErrNoAuth
	}}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go serve(conn, config)
	}
}

func serve(conn net.Conn, config *ssh.ServerConfig) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		conn.Close()
		return
	}
	defer server.Close()
	// keepalive@openssh.com and other global requests get a reply, as OpenSSH does.
	go ssh.DiscardRequests(requests)
	// Like ClientAliveInterval 30 / ClientAliveCountMax 3: drop clients that
	// vanished, so frozen experiment links do not leak ptys and tmux clients.
	go func() {
		missed := 0
		for range time.Tick(30 * time.Second) {
			reply := make(chan error, 1)
			go func() { _, _, err := server.SendRequest("keepalive@openssh.com", true, nil); reply <- err }()
			select {
			case err := <-reply:
				if err != nil {
					return
				}
				missed = 0
			case <-time.After(30 * time.Second):
				if missed++; missed >= 3 {
					server.Close()
					return
				}
			}
		}
	}()
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go session(channel, requests)
	}
}

func session(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	size := &pty.Winsize{Cols: 80, Rows: 24}
	var terminal *os.File
	var mu sync.Mutex
	term := "xterm-256color"
	for request := range requests {
		switch request.Type {
		case "pty-req":
			var payload struct {
				Term          string
				Cols, Rows    uint32
				Width, Height uint32
				Modes         string
			}
			if ssh.Unmarshal(request.Payload, &payload) == nil {
				term = payload.Term
				size = &pty.Winsize{Cols: uint16(payload.Cols), Rows: uint16(payload.Rows)}
			}
			_ = request.Reply(true, nil)
		case "window-change":
			if len(request.Payload) >= 8 {
				mu.Lock()
				size = &pty.Winsize{Cols: uint16(binary.BigEndian.Uint32(request.Payload)), Rows: uint16(binary.BigEndian.Uint32(request.Payload[4:]))}
				if terminal != nil {
					_ = pty.Setsize(terminal, size)
				}
				mu.Unlock()
			}
		case "exec", "shell":
			command := os.Getenv("SHELL")
			if request.Type == "exec" {
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				command = payload.Command
			}
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = append(os.Environ(), "TERM="+term)
			mu.Lock()
			file, err := pty.StartWithSize(cmd, size)
			terminal = file
			mu.Unlock()
			if err != nil {
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
			go func() {
				// The client went away: end the tmux client like sshd does (SIGHUP).
				_, _ = copyTo(file, channel)
				_ = cmd.Process.Signal(syscall.SIGHUP)
			}()
			go func() {
				_, _ = copyTo(channel, file)
				_ = cmd.Wait()
				status := make([]byte, 4)
				_, _ = channel.SendRequest("exit-status", false, status)
				_ = channel.Close()
			}()
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
	if terminal != nil {
		terminal.Close()
	}
}

func copyTo(dst interface{ Write([]byte) (int, error) }, src interface{ Read([]byte) (int, error) }) (int64, error) {
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if _, werr := dst.Write(buffer[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}
