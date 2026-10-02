package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// terminalProcess is a tmux client attached to a pseudo terminal: a Unix PTY
// on macOS/Linux, a ConPTY on Windows (see terminal_unix.go / terminal_windows.go).
type terminalProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
}

type terminalStream struct {
	pty    terminalProcess
	cancel context.CancelFunc
}

type terminalStreams struct {
	mu     sync.Mutex
	active map[string]*terminalStream
	send   func(protocol.Envelope) error
}

func newTerminalStreams(send func(protocol.Envelope) error) *terminalStreams {
	return &terminalStreams{active: make(map[string]*terminalStream), send: send}
}

func (s *terminalStreams) open(request *protocol.StreamOpen) {
	if request == nil || request.StreamID == "" {
		return
	}
	if !protocol.ValidSessionName(request.Session) || request.Cols == 0 || request.Rows == 0 {
		s.reportClose(request.StreamID, "invalid terminal request")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	file, err := startAttach(ctx, request.Session, request.Cols, request.Rows)
	if err != nil {
		cancel()
		s.reportClose(request.StreamID, fmt.Sprintf("cannot open tmux session: %v", err))
		return
	}
	stream := &terminalStream{pty: file, cancel: cancel}
	s.mu.Lock()
	if _, exists := s.active[request.StreamID]; exists {
		s.mu.Unlock()
		stream.cancel()
		_ = file.Close()
		_ = file.Wait()
		return
	}
	s.active[request.StreamID] = stream
	s.mu.Unlock()
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, readErr := file.Read(buffer)
			if count > 0 {
				data := append([]byte(nil), buffer[:count]...)
				if s.send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamData, StreamData: &protocol.StreamData{StreamID: request.StreamID, Data: data}}) != nil {
					cancel()
					_ = file.Close()
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = file.Wait()
		s.mu.Lock()
		current := s.active[request.StreamID]
		if current == stream {
			delete(s.active, request.StreamID)
		}
		s.mu.Unlock()
		if current == stream {
			s.reportClose(request.StreamID, "terminal exited")
		}
		cancel()
		_ = file.Close()
	}()
}

func (s *terminalStreams) reportClose(id, reason string) {
	_ = s.send(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id, Reason: reason}})
}

func (s *terminalStreams) input(data *protocol.StreamData) {
	if data == nil || len(data.Data) > 16<<10 {
		return
	}
	s.mu.Lock()
	stream := s.active[data.StreamID]
	s.mu.Unlock()
	if stream != nil {
		_, _ = stream.pty.Write(data.Data)
	}
}

func (s *terminalStreams) resize(size *protocol.StreamResize) {
	if size == nil || size.Cols == 0 || size.Rows == 0 {
		return
	}
	s.mu.Lock()
	stream := s.active[size.StreamID]
	s.mu.Unlock()
	if stream != nil {
		_ = stream.pty.Resize(size.Cols, size.Rows)
	}
}

func (s *terminalStreams) close(id string) {
	s.mu.Lock()
	stream := s.active[id]
	delete(s.active, id)
	s.mu.Unlock()
	if stream != nil {
		stream.cancel()
		_ = stream.pty.Close()
	}
}

func (s *terminalStreams) closeAll() {
	s.mu.Lock()
	streams := s.active
	s.active = make(map[string]*terminalStream)
	s.mu.Unlock()
	for _, stream := range streams {
		stream.cancel()
		_ = stream.pty.Close()
	}
}
