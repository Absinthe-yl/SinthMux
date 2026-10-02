// Command termcheck drives a SinthMux Hub the way the web page does and checks
// the full terminal path: token login, device online, session create/list,
// terminal ticket, WebSocket attach, input/output, resize, reattach after a
// disconnect (output survives in tmux), ticket replay rejection, and cleanup.
//
//	go run ./tests/e2e/termcheck -hub https://hub.example.com -token smu_... -device "My Mac"
//
// The login token is read from SINTHMUX_E2E_TOKEN when -token is omitted.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var (
	hub      = flag.String("hub", "", "Hub base URL, e.g. https://106.55.15.160")
	token    = flag.String("token", os.Getenv("SINTHMUX_E2E_TOKEN"), "login token (or SINTHMUX_E2E_TOKEN)")
	device   = flag.String("device", "", "device name to test")
	session  = flag.String("session", "e2e-term", "tmux session to create and remove")
	insecure = flag.Bool("insecure", false, "skip TLS verification")
	keep     = flag.Bool("keep", false, "keep the test session")

	client   *http.Client
	csrf     string
	failures int
)

func check(name string, ok bool, detail string) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		failures++
	}
	if detail != "" {
		detail = "  (" + detail + ")"
	}
	fmt.Printf("[%s] %s%s\n", mark, name, detail)
}

func call(method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request, _ := http.NewRequest(method, *hub+path, reader)
	request.Header.Set("Origin", *hub)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set("X-Sinthmux-CSRF", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return response.StatusCode, nil
}

func ticket(deviceID string) (string, error) {
	var result struct{ Ticket string }
	status, err := call(http.MethodPost, "/api/v1/devices/"+url.PathEscape(deviceID)+"/sessions/"+url.PathEscape(*session)+"/ticket", nil, &result)
	if err != nil || status != http.StatusOK || result.Ticket == "" {
		return "", fmt.Errorf("ticket: status %d %v", status, err)
	}
	return result.Ticket, nil
}

type terminal struct {
	conn   *websocket.Conn
	output chan []byte
	closed chan error
	seen   bytes.Buffer
}

func dial(ctx context.Context, t string) (*terminal, *http.Response, error) {
	endpoint := strings.Replace(*hub, "http", "ws", 1) + "/ws/v1/terminal"
	header := http.Header{"Origin": {*hub}}
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: header, Subprotocols: []string{"sinthmux.v1", "sinthmux.ticket." + t}})
	if err != nil {
		return nil, response, err
	}
	conn.SetReadLimit(1 << 20)
	term := &terminal{conn: conn, output: make(chan []byte, 256), closed: make(chan error, 1)}
	go func() {
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				term.closed <- err
				return
			}
			if kind == websocket.MessageBinary {
				term.output <- data
			}
		}
	}()
	return term, response, nil
}

func (t *terminal) send(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.conn.Write(ctx, websocket.MessageBinary, []byte(text))
}

func (t *terminal) resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, _ := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	return t.conn.Write(ctx, websocket.MessageText, data)
}

// waitFor collects output until pattern matches or the timeout passes.
func (t *terminal) waitFor(pattern *regexp.Regexp, timeout time.Duration) ([]string, bool) {
	deadline := time.After(timeout)
	for {
		if match := pattern.FindStringSubmatch(t.seen.String()); match != nil {
			return match, true
		}
		select {
		case data := <-t.output:
			t.seen.Write(data)
		case <-t.closed:
			return nil, false
		case <-deadline:
			return nil, false
		}
	}
}

func main() {
	flag.Parse()
	*hub = strings.TrimRight(*hub, "/")
	if *hub == "" || *token == "" || *device == "" {
		fmt.Fprintln(os.Stderr, "usage: termcheck -hub URL -device NAME [-token TOKEN]")
		os.Exit(2)
	}
	jar, _ := cookiejar.New(nil)
	client = &http.Client{Jar: jar, Timeout: 20 * time.Second}
	if *insecure {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	defer func() {
		if failures > 0 {
			fmt.Printf("\n%d check(s) failed\n", failures)
			os.Exit(1)
		}
		fmt.Println("\nall checks passed")
	}()

	status, err := call(http.MethodPost, "/api/v1/auth/token", map[string]string{"token": *token}, nil)
	check("登录令牌登录", err == nil && status == http.StatusOK, fmt.Sprintf("HTTP %d", status))
	var me struct{ CSRF string }
	_, _ = call(http.MethodGet, "/api/v1/auth/me", nil, &me)
	csrf = me.CSRF
	defer func() { _, _ = call(http.MethodPost, "/api/v1/auth/logout", nil, nil) }()

	var list struct {
		Devices []struct{ ID, Name, Status, Platform string }
	}
	var deviceID, platform string
	for i := 0; i < 20 && deviceID == ""; i++ {
		_, _ = call(http.MethodGet, "/api/v1/devices", nil, &list)
		for _, d := range list.Devices {
			if d.Name == *device && d.Status == "online" {
				deviceID, platform = d.ID, d.Platform
			}
		}
		if deviceID == "" {
			time.Sleep(time.Second)
		}
	}
	check("设备在线", deviceID != "", *device+" / "+platform)
	if deviceID == "" {
		return
	}
	base := "/api/v1/devices/" + url.PathEscape(deviceID) + "/sessions"
	_, _ = call(http.MethodDelete, base+"/"+url.PathEscape(*session), nil, nil)
	status, _ = call(http.MethodPost, base, map[string]string{"name": *session}, nil)
	check("创建 tmux 会话", status == http.StatusCreated, fmt.Sprintf("HTTP %d", status))
	if !*keep {
		defer func() {
			status, _ := call(http.MethodDelete, base+"/"+url.PathEscape(*session), nil, nil)
			check("关闭 tmux 会话", status == http.StatusNoContent || status == http.StatusOK, fmt.Sprintf("HTTP %d", status))
		}()
	}
	var sessions struct {
		Sessions []struct {
			Name    string
			Windows int
		}
	}
	_, _ = call(http.MethodGet, base, nil, &sessions)
	found := false
	for _, s := range sessions.Sessions {
		found = found || (s.Name == *session && s.Windows >= 1)
	}
	check("会话出现在列表中", found, fmt.Sprintf("%d 个会话", len(sessions.Sessions)))

	first, err := ticket(deviceID)
	check("签发终端票据", err == nil, "")
	if err != nil {
		return
	}
	ctx := context.Background()
	term, _, err := dial(ctx, first)
	check("终端 WebSocket 连接", err == nil, errString(err))
	if err != nil {
		return
	}
	marker := fmt.Sprintf("SMX%d", time.Now().UnixNano()%1000000)
	_ = term.resize(100, 30)
	time.Sleep(500 * time.Millisecond)
	_ = term.send("echo " + marker + "-$((6*7))\r")
	_, ok := term.waitFor(regexp.MustCompile(regexp.QuoteMeta(marker)+`-42`), 10*time.Second)
	check("输入命令并收到输出", ok, marker+"-42")

	_ = term.resize(123, 37)
	time.Sleep(700 * time.Millisecond)
	if platform == "windows" {
		_ = term.send("$Host.UI.RawUI.WindowSize.Width;$Host.UI.RawUI.WindowSize.Height\r")
		match, ok := term.waitFor(regexp.MustCompile(`(?m)^\s*(\d+)\s*\r?\n\s*(\d+)\s*$`), 10*time.Second)
		check("调整窗口尺寸生效", ok && match[1] == "123", strings.Join(match, " "))
	} else {
		_ = term.send("stty size\r")
		match, ok := term.waitFor(regexp.MustCompile(`(\d+) (\d+)\r?\n`), 10*time.Second)
		detail := ""
		if match != nil {
			detail = "rows=" + match[1] + " cols=" + match[2]
		}
		// tmux shows the session at the smallest attached client; a status line takes one row.
		check("调整窗口尺寸生效", ok && match[2] == "123" && (match[1] == "36" || match[1] == "37"), detail)
	}
	_ = term.send("echo 中文UTF8-" + marker + "\r")
	_, ok = term.waitFor(regexp.MustCompile("中文UTF8-"+regexp.QuoteMeta(marker)), 10*time.Second)
	check("UTF-8 中文输出", ok, "")
	// Output produced after the browser leaves must still be there on return.
	_ = term.send("sleep 2; echo BACKGROUND-" + marker + "\r")
	_ = term.conn.Close(websocket.StatusNormalClosure, "browser closed")

	time.Sleep(3 * time.Second)
	replay, response, err := dial(ctx, first)
	rejected := err != nil && response != nil && response.StatusCode == http.StatusUnauthorized
	if replay != nil {
		_ = replay.conn.CloseNow()
	}
	check("同一票据不能重复使用", rejected, "")

	second, err := ticket(deviceID)
	check("重新签发票据", err == nil, "")
	if err != nil {
		return
	}
	again, _, err := dial(ctx, second)
	check("断开后重新进入同一会话", err == nil, errString(err))
	if err != nil {
		return
	}
	_ = again.resize(100, 30)
	_, ok = again.waitFor(regexp.MustCompile("BACKGROUND-"+regexp.QuoteMeta(marker)), 10*time.Second)
	check("断开期间的输出仍在会话中", ok, "")
	_ = again.conn.Close(websocket.StatusNormalClosure, "done")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	var close websocket.CloseError
	if errors.As(err, &close) {
		return close.Reason
	}
	return err.Error()
}
