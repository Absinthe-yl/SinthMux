// Command netproxy is the network emulator for tests/experiments. It forwards
// TCP to a target and, per traffic class, adds a one-way delay or cuts the
// network. With -classify the first request line decides the class: requests
// to /ws/v1/connectors and /api/v1/connectors are "device" (connector ↔ Hub),
// everything else is "client" (browser ↔ Hub). Without -classify every
// connection is "client" (used in front of the SSH baseline server).
//
//	/delay?client=10&device=10   one-way delay in ms (RTT grows by twice that)
//	/block?class=client&mode=blackhole
//	        open links of the class stop forever, like a NAT mapping lost on a
//	        network switch; new links are held without data until /unblock,
//	        like SYNs retransmitted until the network returns
//	/block?class=client&mode=refuse
//	        open links freeze the same way; new links are closed at once, like a
//	        device whose interface is down
//	/unblock?class=client         new and held links flow again
//	/unblock?class=device&reset=1 also send RST on the frozen links, like the
//	                              first packet on an old flow after a NAT change
//	/status                       counters
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	open int32 = iota
	blackhole
	refuse
)

type class struct {
	name    string
	delay   atomic.Int64 // one-way, nanoseconds
	mode    atomic.Int32
	opened  atomic.Int64
	refused atomic.Int64
	mu      sync.Mutex
	links   map[*link]bool
	held    []chan struct{}
}

type link struct {
	frozen         atomic.Bool
	client, server net.Conn
}

// reset closes both sockets with an RST, like a peer answering a segment of a
// flow it no longer knows (NAT mapping gone after a network switch).
func (l *link) reset() {
	for _, conn := range []net.Conn{l.client, l.server} {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
}

var classes = map[string]*class{
	"client": {name: "client", links: map[*link]bool{}},
	"device": {name: "device", links: map[*link]bool{}},
}

type chunk struct {
	due  time.Time
	data []byte
}

// pipe copies src to dst; each chunk is released after the class delay, in
// order. A frozen link never delivers again but keeps both sockets open.
func pipe(dst net.Conn, src io.Reader, l *link, c *class, done chan<- struct{}) {
	queue := make(chan chunk, 8192)
	go func() {
		defer func() { done <- struct{}{} }()
		for item := range queue {
			if wait := time.Until(item.due); wait > 0 {
				time.Sleep(wait)
			}
			if l.frozen.Load() {
				select {}
			}
			if _, err := dst.Write(item.data); err != nil {
				return
			}
		}
	}()
	buffer := make([]byte, 32<<10)
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if l.frozen.Load() {
				select {}
			}
			queue <- chunk{due: time.Now().Add(time.Duration(c.delay.Load())), data: append([]byte(nil), buffer[:n]...)}
		}
		if err != nil {
			close(queue)
			return
		}
	}
}

func serve(client net.Conn, target string, classify bool) {
	reader := bufio.NewReaderSize(client, 16<<10)
	c := classes["client"]
	if classify {
		_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
		head, _ := reader.Peek(48)
		_ = client.SetReadDeadline(time.Time{})
		line := string(head)
		if strings.Contains(line, " /ws/v1/connectors") || strings.Contains(line, " /api/v1/connectors") {
			c = classes["device"]
		}
	}
	switch c.mode.Load() {
	case refuse:
		c.refused.Add(1)
		client.Close()
		return
	case blackhole:
		gate := make(chan struct{})
		c.mu.Lock()
		c.held = append(c.held, gate)
		c.mu.Unlock()
		<-gate
	}
	server, err := net.Dial("tcp", target)
	if err != nil {
		client.Close()
		return
	}
	if tcp, ok := server.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	c.opened.Add(1)
	l := &link{client: client, server: server}
	c.mu.Lock()
	c.links[l] = true
	c.mu.Unlock()
	done := make(chan struct{}, 2)
	go pipe(server, reader, l, c, done)
	go pipe(client, server, l, c, done)
	<-done
	if !l.frozen.Load() {
		client.Close()
		server.Close()
	}
	c.mu.Lock()
	delete(c.links, l)
	c.mu.Unlock()
}

func selected(r *http.Request) []*class {
	switch r.URL.Query().Get("class") {
	case "client":
		return []*class{classes["client"]}
	case "device":
		return []*class{classes["device"]}
	default:
		return []*class{classes["client"], classes["device"]}
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18094", "listen address")
	target := flag.String("target", "127.0.0.1:18093", "target address")
	control := flag.String("control", "127.0.0.1:18095", "control HTTP address")
	classify := flag.Bool("classify", false, "split connector traffic into the device class")
	flag.Parse()

	http.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		for name, c := range classes {
			if value := r.URL.Query().Get(name); value != "" {
				ms, _ := strconv.ParseFloat(value, 64)
				c.delay.Store(int64(ms * float64(time.Millisecond)))
			}
		}
		fmt.Fprintf(w, "client %v device %v\n", time.Duration(classes["client"].delay.Load()), time.Duration(classes["device"].delay.Load()))
	})
	http.HandleFunc("/block", func(w http.ResponseWriter, r *http.Request) {
		mode := blackhole
		if r.URL.Query().Get("mode") == "refuse" {
			mode = refuse
		}
		count := 0
		for _, c := range selected(r) {
			c.mode.Store(mode)
			c.mu.Lock()
			for l := range c.links {
				if !l.frozen.Load() {
					l.frozen.Store(true)
					count++
				}
			}
			c.mu.Unlock()
		}
		fmt.Fprintf(w, "froze %d\n", count)
	})
	http.HandleFunc("/unblock", func(w http.ResponseWriter, r *http.Request) {
		count := 0
		reset := r.URL.Query().Get("reset") == "1"
		for _, c := range selected(r) {
			c.mode.Store(open)
			if reset {
				c.mu.Lock()
				for l := range c.links {
					if l.frozen.Load() {
						l.reset()
						delete(c.links, l)
					}
				}
				c.mu.Unlock()
			}
			c.mu.Lock()
			for _, gate := range c.held {
				close(gate)
			}
			count += len(c.held)
			c.held = nil
			c.mu.Unlock()
		}
		fmt.Fprintf(w, "released %d\n", count)
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		for _, name := range []string{"client", "device"} {
			c := classes[name]
			c.mu.Lock()
			fmt.Fprintf(w, "%s open %d opened %d refused %d held %d mode %d delay %v\n", name, len(c.links), c.opened.Load(), c.refused.Load(), len(c.held), c.mode.Load(), time.Duration(c.delay.Load()))
			c.mu.Unlock()
		}
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
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go serve(client, *target, *classify)
	}
}
