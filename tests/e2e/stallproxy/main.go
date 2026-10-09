// Command stallproxy forwards TCP to a target and can freeze the terminal
// WebSockets (/ws/v1/terminal) that are currently open: bytes stop flowing in
// both directions but the sockets stay open, like a NAT mapping that silently
// expired. Plain HTTP and new connections keep working, so a client that
// notices the stall can fetch a new ticket and reconnect.
//
//	go run ./tests/e2e/stallproxy -listen 127.0.0.1:15174 -target 127.0.0.1:15173 -control 127.0.0.1:15175
//	curl http://127.0.0.1:15175/freeze    # freeze open connections
//	curl http://127.0.0.1:15175/status    # open / frozen counts
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

type link struct {
	terminal bool
	frozen   atomic.Bool
	gate     chan struct{} // closed when frozen; copy loops then block forever
	once     sync.Once
}

func (l *link) freeze() { l.once.Do(func() { l.frozen.Store(true); close(l.gate) }) }

type gatedWriter struct {
	io.Writer
	l *link
}

func (w gatedWriter) Write(p []byte) (int, error) {
	if w.l.frozen.Load() {
		select {} // hold the data and the socket, never deliver
	}
	return w.Writer.Write(p)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:15174", "listen address")
	target := flag.String("target", "127.0.0.1:15173", "target address")
	control := flag.String("control", "127.0.0.1:15175", "control HTTP address")
	flag.Parse()

	var mu sync.Mutex
	open := map[*link]bool{}
	var frozen atomic.Int64

	http.HandleFunc("/freeze", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count := 0
		for l := range open {
			if l.terminal && !l.frozen.Load() {
				l.freeze()
				frozen.Add(1)
				count++
			}
		}
		mu.Unlock()
		fmt.Fprintf(w, "froze %d\n", count)
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		terminals := 0
		for l := range open {
			if l.terminal {
				terminals++
			}
		}
		fmt.Fprintf(w, "open %d terminals %d frozen %d\n", len(open), terminals, frozen.Load())
	})
	go func() { log.Fatal(http.ListenAndServe(*control, nil)) }()

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		client, err := listener.Accept()
		if err != nil {
			continue
		}
		go func() {
			server, err := net.Dial("tcp", *target)
			if err != nil {
				client.Close()
				return
			}
			// Peek at the request line to recognise terminal WebSockets.
			reader := bufio.NewReaderSize(client, 8192)
			head, _ := reader.Peek(64)
			l := &link{gate: make(chan struct{}), terminal: strings.HasPrefix(string(head), "GET /ws/v1/terminal")}
			mu.Lock()
			open[l] = true
			mu.Unlock()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(gatedWriter{server, l}, reader); done <- struct{}{} }()
			go func() { _, _ = io.Copy(gatedWriter{client, l}, server); done <- struct{}{} }()
			<-done
			if !l.frozen.Load() {
				client.Close()
				server.Close()
			}
			mu.Lock()
			delete(open, l)
			mu.Unlock()
		}()
	}
}
