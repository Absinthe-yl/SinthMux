package main

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type writeFake struct {
	mu      sync.Mutex
	max     int
	zero    bool
	fail    error
	writes  []byte
	closes  int
	started chan struct{}
	release chan struct{}
}

func (f *writeFake) Read([]byte) (int, error) { return 0, io.EOF }
func (f *writeFake) Write(p []byte) (int, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.zero {
		return 0, nil
	}
	n := len(p)
	if f.max > 0 && n > f.max {
		n = f.max
	}
	f.writes = append(f.writes, p[:n]...)
	if f.fail != nil {
		return n, f.fail
	}
	return n, nil
}
func (f *writeFake) Close() error                { f.mu.Lock(); f.closes++; f.mu.Unlock(); return nil }
func (f *writeFake) Resize(uint16, uint16) error { return nil }
func (f *writeFake) Wait() error                 { return nil }
func (f *writeFake) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.writes...)
}
func addFake(s *terminalStreams, id string, f *writeFake) *terminalStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &terminalStream{pty: f, cancel: func() {}}
	s.active[id] = st
	return st
}
func TestTerminalInputCompletesShortWrites(t *testing.T) {
	f := &writeFake{max: 2}
	s := newTerminalStreams(func(protocol.Envelope) error { return nil })
	addFake(s, "x", f)
	s.input(&protocol.StreamData{StreamID: "x", Data: []byte("abcdef")})
	if got := string(f.bytes()); got != "abcdef" {
		t.Fatalf("writes=%q", got)
	}
}
func TestTerminalInputShortWritesPreserveUTF8Bytes(t *testing.T) {
	f := &writeFake{max: 1}
	s := newTerminalStreams(func(protocol.Envelope) error { return nil })
	addFake(s, "x", f)
	data := []byte("前缀-中文-后缀")
	s.input(&protocol.StreamData{StreamID: "x", Data: data})
	if got := string(f.bytes()); got != string(data) {
		t.Fatalf("writes=%q want %q", got, data)
	}
}
func TestTerminalInputZeroWriteClosesWithReason(t *testing.T) {
	var got []protocol.Envelope
	f := &writeFake{zero: true}
	s := newTerminalStreams(func(e protocol.Envelope) error { got = append(got, e); return nil })
	addFake(s, "x", f)
	s.input(&protocol.StreamData{StreamID: "x", Data: []byte("abc")})
	if len(got) != 1 || got[0].StreamClose == nil || got[0].StreamClose.Reason != "terminal input failed: short write" {
		t.Fatalf("close=%v", got)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}
func TestTerminalInputPartialErrorClosesWithoutReplay(t *testing.T) {
	var got []protocol.Envelope
	f := &writeFake{max: 2, fail: errors.New("broken")}
	s := newTerminalStreams(func(e protocol.Envelope) error { got = append(got, e); return nil })
	addFake(s, "x", f)
	s.input(&protocol.StreamData{StreamID: "x", Data: []byte("abcdef")})
	if got := string(f.bytes()); got != "ab" {
		t.Fatalf("writes=%q", got)
	}
	if len(got) != 1 || got[0].StreamClose == nil || got[0].StreamClose.Reason != "terminal input failed: broken" {
		t.Fatalf("close=%v", got)
	}
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
}
func TestTerminalInputFailureDoesNotCloseReplacement(t *testing.T) {
	var got []protocol.Envelope
	old := &writeFake{fail: errors.New("old broken"), started: make(chan struct{}, 1), release: make(chan struct{})}
	fresh := &writeFake{}
	s := newTerminalStreams(func(e protocol.Envelope) error { got = append(got, e); return nil })
	oldStream := addFake(s, "x", old)
	done := make(chan struct{})
	go func() { s.input(&protocol.StreamData{StreamID: "x", Data: []byte("old")}); close(done) }()
	<-old.started
	s.mu.Lock()
	newStream := &terminalStream{pty: fresh, cancel: func() {}}
	s.active["x"] = newStream
	s.mu.Unlock()
	close(old.release)
	<-done
	if old.closes != 1 || fresh.closes != 0 || len(got) != 0 {
		t.Fatalf("old close=%d fresh close=%d notifications=%d", old.closes, fresh.closes, len(got))
	}
	s.input(&protocol.StreamData{StreamID: "x", Data: []byte("new")})
	if string(fresh.bytes()) != "new" || s.active["x"] != newStream {
		t.Fatalf("replacement writes=%q active=%v", fresh.bytes(), s.active["x"])
	}
	_ = oldStream
}
func TestTerminalOutputFailureDoesNotNotifyReplacement(t *testing.T) {
	var got []protocol.Envelope
	old := &writeFake{}
	fresh := &writeFake{}
	s := newTerminalStreams(func(e protocol.Envelope) error { got = append(got, e); return nil })
	oldStream := addFake(s, "x", old)
	s.mu.Lock()
	s.active["x"] = &terminalStream{pty: fresh, cancel: func() {}}
	s.mu.Unlock()
	s.terminate("x", oldStream, "terminal output failed: send", true)
	if old.closes != 1 || fresh.closes != 0 || len(got) != 0 {
		t.Fatalf("old close=%d fresh close=%d notifications=%d", old.closes, fresh.closes, len(got))
	}
}
func TestTerminalFailureClosesAndReportsOnce(t *testing.T) {
	var got []protocol.Envelope
	f := &writeFake{zero: true}
	s := newTerminalStreams(func(e protocol.Envelope) error { got = append(got, e); return nil })
	addFake(s, "x", f)
	s.input(&protocol.StreamData{StreamID: "x", Data: []byte("bad")})
	s.close("x")
	s.closeAll()
	if f.closes != 1 {
		t.Fatalf("closes=%d", f.closes)
	}
	if len(got) != 1 {
		t.Fatalf("notifications=%d", len(got))
	}
}
