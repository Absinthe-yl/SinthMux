// Command stallcheck verifies that the Hub releases a terminal whose browser
// silently disappeared: it opens a terminal over raw TCP, completes the
// WebSocket handshake and then never reads or answers pings. The Hub must drop
// the stream within 45 seconds, which detaches the tmux client on the device.
// It runs on the same machine as the device (it inspects tmux directly).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	hub    = flag.String("hub", "", "plain-HTTP Hub URL on this machine")
	token  = flag.String("token", "", "owner login token")
	device = flag.String("device", "", "device name")
	socket = flag.String("socket", "", "tmux -L socket the device connector uses")
)

func main() {
	flag.Parse()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	post := func(path string, body any, csrf string) (*http.Response, error) {
		data, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, *hub+path, bytes.NewReader(data))
		request.Header.Set("Origin", *hub)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Sinthmux-CSRF", csrf)
		return client.Do(request)
	}
	fail := func(format string, args ...any) {
		fmt.Printf("[FAIL] P2  "+format+"\n", args...)
		os.Exit(1)
	}
	if response, err := post("/api/v1/auth/token", map[string]string{"token": *token}, ""); err != nil || response.StatusCode != 200 {
		fail("login")
	}
	var me struct{ CSRF string }
	response, _ := client.Get(*hub + "/api/v1/auth/me")
	_ = json.NewDecoder(response.Body).Decode(&me)
	var list struct{ Devices []struct{ ID, Name string } }
	response, _ = client.Get(*hub + "/api/v1/devices")
	_ = json.NewDecoder(response.Body).Decode(&list)
	deviceID := ""
	for _, d := range list.Devices {
		if d.Name == *device {
			deviceID = d.ID
		}
	}
	session := "e2e-stall"
	base := "/api/v1/devices/" + url.PathEscape(deviceID) + "/sessions"
	_, _ = post(base, map[string]string{"name": session}, me.CSRF)
	defer func() {
		request, _ := http.NewRequest(http.MethodDelete, *hub+base+"/"+session, nil)
		request.Header.Set("Origin", *hub)
		request.Header.Set("X-Sinthmux-CSRF", me.CSRF)
		_, _ = client.Do(request)
	}()
	response, err := post(base+"/"+session+"/ticket", nil, me.CSRF)
	if err != nil || response.StatusCode != 200 {
		fail("ticket")
	}
	var ticket struct{ Ticket string }
	_ = json.NewDecoder(response.Body).Decode(&ticket)

	address := strings.TrimPrefix(*hub, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		fail("dial: %v", err)
	}
	defer conn.Close()
	var cookies []string
	for _, c := range jar.Cookies(&url.URL{Scheme: "http", Host: address}) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	fmt.Fprintf(conn, "GET /ws/v1/terminal HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nCookie: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: sinthmux.v1, sinthmux.ticket.%s\r\n\r\n", address, *hub, strings.Join(cookies, "; "), ticket.Ticket)
	status, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(status, "101") {
		fail("handshake: %q", status)
	}
	// Read nothing from now on; shrink the receive buffer so pings back up.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	attached := false
	for i := 0; i < 50 && !attached; i++ {
		time.Sleep(100 * time.Millisecond)
		out, _ := exec.Command("tmux", "-L", *socket, "list-clients", "-t", "="+session).Output()
		attached = strings.TrimSpace(string(out)) != ""
	}
	if !attached {
		fail("tmux client never attached")
	}
	start := time.Now()
	for time.Since(start) < 60*time.Second {
		out, _ := exec.Command("tmux", "-L", *socket, "list-clients", "-t", "="+session).Output()
		if strings.TrimSpace(string(out)) == "" {
			elapsed := time.Since(start)
			mark := "PASS"
			if elapsed > 45*time.Second {
				mark = "FAIL"
			}
			fmt.Printf("[%s] P2  浏览器静默失联后 Hub 释放 tmux 客户端  (%v)\n", mark, elapsed.Round(time.Second))
			if mark == "FAIL" {
				os.Exit(1)
			}
			return
		}
		time.Sleep(time.Second)
	}
	fail("tmux client still attached after 60 s")
}
