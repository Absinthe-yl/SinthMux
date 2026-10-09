// Command featurecheck verifies the tier-1 features end to end against a real
// Hub and device: file upload, terminal history export, session notifications
// and the terminal heartbeat. Results on the device are read back through a
// real terminal session, so the same run works for local and remote devices.
//
//	go run ./tests/e2e/featurecheck -hub https://hub.example.com -device "My Mac" [-viewer-token T]
//
// The owner login token is read from SINTHMUX_E2E_TOKEN when -token is omitted.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	hub         = flag.String("hub", "", "Hub base URL")
	token       = flag.String("token", os.Getenv("SINTHMUX_E2E_TOKEN"), "owner login token (or SINTHMUX_E2E_TOKEN)")
	viewerToken = flag.String("viewer-token", os.Getenv("SINTHMUX_E2E_VIEWER_TOKEN"), "optional viewer login token for permission checks")
	device      = flag.String("device", "", "device name to test")
	session     = flag.String("session", "e2e-feat", "tmux session to create and remove")
	insecure    = flag.Bool("insecure", false, "skip TLS verification")
	skip        = flag.String("skip", "", "comma-separated check IDs to skip, e.g. U7,X6,N6,P2")
	only        = flag.String("only", "", "comma-separated check IDs to run; all others are skipped")
	restartCmd  = flag.String("restart", "", "shell command run on the device (through the terminal) to restart the connector for N6")
	big         = flag.Bool("big", true, "run the 60 000 line export (X6)")
	failures    int
	skipped     = map[string]bool{}
	selected    = map[string]bool{}
)

func check(id, name string, ok bool, detail string) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		failures++
	}
	if detail != "" {
		detail = "  (" + detail + ")"
	}
	fmt.Printf("[%s] %-3s %s%s\n", mark, id, name, detail)
}

func want(id string) bool {
	if skipped[id] || len(selected) > 0 && !selected[id] {
		fmt.Printf("[SKIP] %-3s\n", id)
		return false
	}
	return true
}

type client struct {
	http *http.Client
	csrf string
}

func login(secret string) (*client, error) {
	jar, _ := cookiejar.New(nil)
	c := &client{http: &http.Client{Jar: jar, Timeout: 120 * time.Second}}
	if *insecure {
		c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	status, _, err := c.do(http.MethodPost, "/api/v1/auth/token", "application/json", strings.NewReader(`{"token":"`+secret+`"}`), -1)
	if err != nil || status != http.StatusOK {
		return nil, fmt.Errorf("login: HTTP %d %v", status, err)
	}
	var me struct{ CSRF string }
	if err := c.json(http.MethodGet, "/api/v1/auth/me", nil, &me); err != nil {
		return nil, err
	}
	c.csrf = me.CSRF
	return c, nil
}

func (c *client) do(method, path, contentType string, body io.Reader, length int64) (int, []byte, error) {
	request, err := http.NewRequest(method, *hub+path, body)
	if err != nil {
		return 0, nil, err
	}
	if length >= 0 {
		request.ContentLength = length
	}
	request.Header.Set("Origin", *hub)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.csrf != "" {
		request.Header.Set("X-Sinthmux-CSRF", c.csrf)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func (c *client) json(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}
	status, data, err := c.do(method, path, "application/json", body, -1)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("HTTP %d: %s", status, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type notification struct{ Session, Color, Message string }

type deviceInfo struct {
	ID, Name, Status, Platform string
	Capabilities               []string
	Notifications              []notification
}

func (c *client) device() (deviceInfo, error) {
	var list struct{ Devices []deviceInfo }
	if err := c.json(http.MethodGet, "/api/v1/devices", nil, &list); err != nil {
		return deviceInfo{}, err
	}
	for _, d := range list.Devices {
		if d.Name == *device {
			return d, nil
		}
	}
	return deviceInfo{}, errors.New("device not found")
}

func (c *client) upload(deviceID, name string, data []byte) (int, map[string]any) {
	status, body, err := c.do(http.MethodPost, "/api/v1/devices/"+url.PathEscape(deviceID)+"/uploads?name="+url.QueryEscape(name), "application/octet-stream", bytes.NewReader(data), int64(len(data)))
	result := map[string]any{}
	_ = json.Unmarshal(body, &result)
	if err != nil {
		result["error"] = err.Error()
	}
	return status, result
}

func (c *client) export(deviceID, sessionName, lines string) (int, http.Header, []byte, error) {
	request, _ := http.NewRequest(http.MethodGet, *hub+"/api/v1/devices/"+url.PathEscape(deviceID)+"/sessions/"+url.PathEscape(sessionName)+"/scrollback?lines="+lines, nil)
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, response.Header, data, err
}

// terminal is a browser-like terminal connection used to run commands on the device.
type terminal struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	seen   bytes.Buffer
	texts  chan string
	closed chan struct{}
}

func (c *client) open(deviceID, sessionName string) (*terminal, error) {
	var result struct{ Ticket string }
	if err := c.json(http.MethodPost, "/api/v1/devices/"+url.PathEscape(deviceID)+"/sessions/"+url.PathEscape(sessionName)+"/ticket", nil, &result); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(*hub, "http", "ws", 1)+"/ws/v1/terminal", &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"Origin": {*hub}}, Subprotocols: []string{"sinthmux.v1", "sinthmux.ticket." + result.Ticket}})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	t := &terminal{conn: conn, texts: make(chan string, 16), closed: make(chan struct{})}
	go func() {
		defer close(t.closed)
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if kind == websocket.MessageText {
				t.texts <- string(data)
				continue
			}
			t.mu.Lock()
			t.seen.Write(data)
			t.mu.Unlock()
		}
	}()
	_ = t.write(websocket.MessageText, `{"type":"resize","cols":200,"rows":50}`)
	return t, nil
}

func (t *terminal) write(kind websocket.MessageType, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.conn.Write(ctx, kind, []byte(text))
}

var runSeq int

// ansi matches terminal control sequences tmux uses while repainting.
var ansi = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z]|[=>78DEHM])`)

// run executes a shell command and returns its output. tmux repaints the
// screen rather than streaming raw text, so the output travels as one line of
// base64 between markers; the markers are built by printf so the echoed
// command line itself can never match.
func (t *terminal) run(command string, timeout time.Duration) (string, bool) {
	runSeq++
	tag := fmt.Sprintf("Q%d%d", time.Now().UnixNano()%100000, runSeq)
	t.mu.Lock()
	t.seen.Reset()
	t.mu.Unlock()
	_ = t.write(websocket.MessageBinary, fmt.Sprintf("printf '%%s-B[%%s]%%s-E\\n' %s \"$( { %s ; } 2>&1 | base64 | tr -d '\\n')\" %s\r", tag, command, tag))
	pattern := regexp.MustCompile(tag + `-B\[([A-Za-z0-9+/=]*)\]` + tag + `-E`)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		screen := ansi.ReplaceAllString(t.seen.String(), "")
		t.mu.Unlock()
		screen = strings.NewReplacer("\r", "", "\n", "").Replace(screen)
		if match := pattern.FindStringSubmatch(screen); match != nil {
			decoded, err := base64.StdEncoding.DecodeString(match[1])
			return strings.TrimSpace(string(decoded)), err == nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", false
}

// exec types a command whose output must stay in the pane (history tests) and
// waits for a marker the shell computes after it finishes.
func (t *terminal) exec(command string, timeout time.Duration) bool {
	runSeq++
	tag := fmt.Sprintf("X%d%d", time.Now().UnixNano()%100000, runSeq)
	t.mu.Lock()
	t.seen.Reset()
	t.mu.Unlock()
	_ = t.write(websocket.MessageBinary, fmt.Sprintf("%s; echo %s-$((40+2))\r", command, tag))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		screen := ansi.ReplaceAllString(t.seen.String(), "")
		t.mu.Unlock()
		if strings.Contains(screen, tag+"-42") {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func randomBytes(n int) []byte {
	data := make([]byte, n)
	_, _ = rand.Read(data)
	// Make sure every byte value appears.
	for i := 0; i < 256 && i < n; i++ {
		data[i] = byte(i)
	}
	return data
}

func sha(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func waitNotification(c *client, accept func([]notification) bool, timeout time.Duration) ([]notification, time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < timeout {
		if d, err := c.device(); err == nil && accept(d.Notifications) {
			return d.Notifications, time.Since(start), true
		}
		time.Sleep(250 * time.Millisecond)
	}
	d, _ := c.device()
	return d.Notifications, time.Since(start), false
}

func findNotification(items []notification, sessionName string) *notification {
	for i := range items {
		if items[i].Session == sessionName {
			return &items[i]
		}
	}
	return nil
}

func main() {
	flag.Parse()
	*hub = strings.TrimRight(*hub, "/")
	for _, id := range strings.Split(*skip, ",") {
		if id = strings.TrimSpace(id); id != "" {
			skipped[id] = true
		}
	}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	if *hub == "" || *token == "" || *device == "" {
		fmt.Fprintln(os.Stderr, "usage: featurecheck -hub URL -device NAME [-token T] [-viewer-token T] [-skip U7,X6]")
		os.Exit(2)
	}
	defer func() {
		if failures > 0 {
			fmt.Printf("\n%d check(s) failed\n", failures)
			os.Exit(1)
		}
		fmt.Println("\nall checks passed")
	}()

	owner, err := login(*token)
	check("--", "登录", err == nil, errString(err))
	if err != nil {
		return
	}
	var dev deviceInfo
	for i := 0; i < 20; i++ {
		if dev, err = owner.device(); err == nil && dev.Status == "online" {
			break
		}
		time.Sleep(time.Second)
	}
	check("--", "设备在线", dev.Status == "online", dev.Name+" / "+dev.Platform+" caps="+strings.Join(dev.Capabilities, ","))
	if dev.Status != "online" {
		return
	}
	base := "/api/v1/devices/" + url.PathEscape(dev.ID) + "/sessions"
	_, _, _ = owner.do(http.MethodDelete, base+"/"+*session, "", nil, -1)
	if err := owner.json(http.MethodPost, base, map[string]string{"name": *session}, nil); err != nil {
		check("--", "创建测试会话", false, err.Error())
		return
	}
	defer func() { _, _, _ = owner.do(http.MethodDelete, base+"/"+*session, "", nil, -1) }()
	term, err := owner.open(dev.ID, *session)
	check("--", "打开测试终端", err == nil, errString(err))
	if err != nil {
		return
	}
	defer term.conn.CloseNow()
	time.Sleep(800 * time.Millisecond)
	// Disable shell history expansion and prompts that could get in the way.
	term.run("set +H 2>/dev/null; true", 10*time.Second)
	sumCommand := "shasum -a 256"
	if out, _ := term.run("command -v sha256sum >/dev/null && echo yes || echo no", 10*time.Second); out == "yes" {
		sumCommand = "sha256sum"
	}

	// ---------- Uploads ----------
	if want("U1") {
		data := randomBytes(5<<20 + 123)
		status, result := owner.upload(dev.ID, "e2e 截图.png", data)
		path, _ := result["path"].(string)
		ok := status == http.StatusCreated && regexp.MustCompile(`/\.sinthmux/uploads/\d{8}-\d{6}-[0-9a-f]{6}-[A-Za-z0-9._-]+$`).MatchString(path)
		remote, _ := term.run(sumCommand+" "+path+" | cut -c1-64", 30*time.Second)
		check("U1", "上传 5 MiB 二进制，设备上哈希一致", ok && remote == sha(data), fmt.Sprintf("HTTP %d %s", status, path))
	}
	if want("U2") {
		status, result := owner.upload(dev.ID, "../../etc/中文 a b.png", []byte("hello"))
		path, _ := result["path"].(string)
		modes, _ := term.run("stat -c '%a' "+path+" 2>/dev/null || stat -f '%Lp' "+path+"; stat -c '%a' $(dirname "+path+") 2>/dev/null || stat -f '%Lp' $(dirname "+path+")", 10*time.Second)
		ok := status == http.StatusCreated && strings.HasSuffix(path, "-___a_b.png") && strings.Contains(path, "/.sinthmux/uploads/") && strings.Join(strings.Fields(modes), ",") == "600,700"
		check("U2", "恶意文件名被清洗，文件 600 / 目录 700", ok, path+" modes="+strings.Join(strings.Fields(modes), ","))
	}
	if want("U3") {
		data := randomBytes(20 << 20)
		status, result := owner.upload(dev.ID, "max.bin", data)
		path, _ := result["path"].(string)
		remote, _ := term.run(sumCommand+" "+path+" | cut -c1-64", 60*time.Second)
		check("U3", "恰好 20 MiB 上传成功", status == http.StatusCreated && remote == sha(data), fmt.Sprintf("HTTP %d", status))
	}
	partials := func() string {
		out, _ := term.run("ls -a ~/.sinthmux/uploads | grep -c '\\.part$' || true", 10*time.Second)
		return out
	}
	if want("U4") {
		status, _, _ := owner.do(http.MethodPost, "/api/v1/devices/"+dev.ID+"/uploads?name=big", "application/octet-stream", bytes.NewReader(make([]byte, 20<<20+1)), 20<<20+1)
		check("U4", "20 MiB + 1 字节被拒绝 413，无残留", status == http.StatusRequestEntityTooLarge && partials() == "0", fmt.Sprintf("HTTP %d", status))
	}
	if want("U5") {
		status, _, _ := owner.do(http.MethodPost, "/api/v1/devices/"+dev.ID+"/uploads?name=x", "application/octet-stream", io.MultiReader(strings.NewReader("abc")), -1)
		check("U5", "无 Content-Length 被拒绝 411", status == http.StatusLengthRequired, fmt.Sprintf("HTTP %d", status))
	}
	if want("U6") {
		// Announce 4 MiB, send 1 MiB, then drop the connection.
		address := strings.TrimPrefix(strings.TrimPrefix(*hub, "http://"), "https://")
		var conn net.Conn
		if strings.HasPrefix(*hub, "https://") {
			conn, err = tls.Dial("tcp", hostPort(address, "443"), &tls.Config{InsecureSkipVerify: *insecure})
		} else {
			conn, err = net.Dial("tcp", hostPort(address, "80"))
		}
		if err == nil {
			cookies := owner.http.Jar.Cookies(mustURL(*hub))
			var cookie []string
			for _, c := range cookies {
				cookie = append(cookie, c.Name+"="+c.Value)
			}
			fmt.Fprintf(conn, "POST /api/v1/devices/%s/uploads?name=cut HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nCookie: %s\r\nX-Sinthmux-CSRF: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n", dev.ID, address, *hub, strings.Join(cookie, "; "), owner.csrf, 4<<20)
			_, _ = conn.Write(make([]byte, 1<<20))
			time.Sleep(500 * time.Millisecond)
			conn.Close()
		}
		// Behind a reverse proxy the Hub learns about the drop a little later.
		start, left := time.Now(), "?"
		for time.Since(start) < 15*time.Second {
			time.Sleep(time.Second)
			if left = partials(); left == "0" {
				break
			}
		}
		check("U6", "上传中途断开，设备上无 .part 残留", err == nil && left == "0", fmt.Sprintf("%v left=%s %s", time.Since(start).Round(100*time.Millisecond), left, errString(err)))
	}
	if want("U7") {
		data := randomBytes(8 << 20)
		statuses := make([]int, 3)
		var wait sync.WaitGroup
		for i := range statuses {
			wait.Add(1)
			go func(i int) { defer wait.Done(); statuses[i], _ = owner.upload(dev.ID, fmt.Sprintf("c%d.bin", i), data) }(i)
		}
		wait.Wait()
		created, limited := 0, 0
		for _, s := range statuses {
			if s == http.StatusCreated {
				created++
			}
			if s == http.StatusTooManyRequests {
				limited++
			}
		}
		check("U7", "同一设备并发上传上限 2", created == 2 && limited == 1, fmt.Sprint(statuses))
	}
	if want("U9") {
		done := make(chan struct{})
		go func() { owner.upload(dev.ID, "bg.bin", randomBytes(10<<20)); close(done) }()
		worst, failed := time.Duration(0), ""
		for i := 0; i < 5; i++ {
			start := time.Now()
			out, ok := term.run(fmt.Sprintf("echo ping%d", i), 10*time.Second)
			ok = ok && out == fmt.Sprintf("ping%d", i)
			if elapsed := time.Since(start); !ok || elapsed > worst {
				worst = elapsed
				if !ok {
					worst = time.Hour
					failed = fmt.Sprintf("ping%d got %q", i, out)
				}
			}
		}
		<-done
		check("U9", "上传期间终端回显不被阻塞（< 1 s）", worst < time.Second, worst.Round(time.Millisecond).String()+" "+failed)
	}

	// ---------- Export ----------
	exportSession := *session + "-x"
	_, _, _ = owner.do(http.MethodDelete, base+"/"+exportSession, "", nil, -1)
	if err := owner.json(http.MethodPost, base, map[string]string{"name": exportSession}, nil); err == nil {
		defer func() { _, _, _ = owner.do(http.MethodDelete, base+"/"+exportSession, "", nil, -1) }()
		xterm, err := owner.open(dev.ID, exportSession)
		if err == nil {
			time.Sleep(800 * time.Millisecond)
			xterm.exec(`for i in $(seq 1 1500); do echo "L$i 中文🙂"; done`, 30*time.Second)
			if want("X1") {
				status, header, data, err := owner.export(dev.ID, exportSession, "all")
				text := string(data)
				ordered := true
				last := -1
				for i := 1; i <= 1500; i++ {
					index := strings.Index(text, fmt.Sprintf("\nL%d 中文🙂\n", i))
					if index < 0 || index < last {
						ordered = false
						break
					}
					last = index
				}
				check("X1", "导出全部历史：1500 行完整有序、UTF-8 正确", err == nil && status == 200 && header.Get("Content-Type") == "text/plain; charset=utf-8" && ordered, fmt.Sprintf("HTTP %d %d bytes", status, len(data)))
			}
			if want("X2") {
				_, _, data, _ := owner.export(dev.ID, exportSession, "1000")
				text := string(data)
				check("X2", "导出最近 1000 行不含最早的行", strings.Contains(text, "\nL1500 中文") && !strings.Contains(text, "\nL1 中文") && !strings.Contains(text, "\nL400 中文"), fmt.Sprintf("%d bytes", len(data)))
			}
			if want("X3") {
				_ = xterm.write(websocket.MessageBinary, "seq 1 300 | less\r")
				time.Sleep(time.Second)
				status, _, data, _ := owner.export(dev.ID, exportSession, "all")
				_ = xterm.write(websocket.MessageBinary, "q")
				time.Sleep(500 * time.Millisecond)
				// tmux cannot capture the normal screen saved behind an alternate
				// screen program, so the last screenful is hidden; history stays.
				check("X3", "less 打开时导出仍包含历史", strings.Contains(string(data), "\nL1000 中文🙂\n"), fmt.Sprintf("HTTP %d %d bytes tail=%q", status, len(data), tail(string(data), 80)))
			}
			if want("X6") && *big {
				xterm.exec("tmux set-option history-limit 70000; tmux new-window", 10*time.Second)
				time.Sleep(time.Second)
				// The new window is now current; generate in it without echo noise.
				_ = xterm.write(websocket.MessageBinary, `python3 -c "import sys;[sys.stdout.write(str(i).rjust(8)+' '+'x'*180+'\n') for i in range(60000)]"; echo BIG-$((40+2))`+"\r")
				deadline := time.Now().Add(60 * time.Second)
				for time.Now().Before(deadline) {
					xterm.mu.Lock()
					doneBig := strings.Contains(xterm.seen.String(), "BIG-42")
					xterm.mu.Unlock()
					if doneBig {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				start := time.Now()
				status, header, data, err := owner.export(dev.ID, exportSession, "all")
				elapsed := time.Since(start)
				ok := err == nil && status == 200 && fmt.Sprint(len(data)) == header.Get("X-Sinthmux-Export-Size") && strings.Contains(string(data), "   59999 x")
				// Data crosses device -> Hub and Hub -> this client; the slower
				// link bounds the time. Measure both with a static download of the
				// connector binary (the device fetches it from the Hub too) and
				// allow 1.5x that transfer time plus 3 s, but at least 15 s.
				budget := 15 * time.Second
				rate := downloadRate()
				if deviceRate := deviceDownloadRate(xterm); deviceRate > 0 && (rate == 0 || deviceRate < rate) {
					rate = deviceRate
				}
				if rate > 0 {
					if linkTime := time.Duration(float64(len(data))/rate*1.5*float64(time.Second)) + 3*time.Second; linkTime > budget {
						budget = linkTime
					}
				}
				check("X6", "6 万行大历史导出", ok && elapsed <= budget, fmt.Sprintf("%.1f MiB in %v, budget %v at %.0f KiB/s", float64(len(data))/(1<<20), elapsed.Round(10*time.Millisecond), budget.Round(100*time.Millisecond), rate/1024))
			}
			xterm.conn.CloseNow()
		}
	}
	if want("X4") {
		status, _, _, _ := owner.export(dev.ID, "no-such-session", "all")
		check("X4", "导出不存在的会话 404", status == http.StatusNotFound, fmt.Sprintf("HTTP %d", status))
	}

	// ---------- Notifications ----------
	notify := "~/.local/bin/sinthmux-connector"
	// Use the running connector's binary: a process whose whole command line is
	// a path ending in /sinthmux-connector (the probing shell has more words).
	if path, ok := term.run("ps -eo args= | awk 'NF == 1 && $1 ~ /\\/sinthmux-connector$/ { print $1; exit }'", 10*time.Second); ok && path != "" {
		notify = path
	}
	if bin := os.Getenv("SINTHMUX_E2E_NOTIFY_BIN"); bin != "" {
		notify = bin
	}
	tag := fmt.Sprintf("e2e-%d", time.Now().UnixNano()%100000)
	if want("N1") {
		start := time.Now()
		term.run(notify+" notify --color red '"+tag+"'", 10*time.Second)
		items, _, ok := waitNotification(owner, func(items []notification) bool {
			n := findNotification(items, *session)
			return n != nil && n.Color == "red" && n.Message == tag
		}, 15*time.Second)
		check("N1", "notify 后网页设备列表出现提醒", ok, fmt.Sprintf("%v %v", time.Since(start).Round(100*time.Millisecond), items))
	}
	if want("N2") {
		term.run(notify+" notify --color green '第二条 中文'", 10*time.Second)
		_, elapsed, ok := waitNotification(owner, func(items []notification) bool {
			n := findNotification(items, *session)
			return n != nil && n.Color == "green" && n.Message == "第二条 中文"
		}, 15*time.Second)
		check("N2", "新提醒覆盖旧提醒，中文正确", ok, elapsed.Round(100*time.Millisecond).String())
	}
	if want("N3") {
		status, _, _ := owner.do(http.MethodDelete, base+"/"+*session+"/notification", "", nil, -1)
		_, elapsed, ok := waitNotification(owner, func(items []notification) bool { return findNotification(items, *session) == nil }, 15*time.Second)
		check("N3", "网页清除提醒", status == http.StatusNoContent && ok, fmt.Sprintf("HTTP %d %v", status, elapsed.Round(100*time.Millisecond)))
	}
	if want("N4") {
		term.run(notify+" notify --color yellow open-clears", 10*time.Second)
		waitNotification(owner, func(items []notification) bool { return findNotification(items, *session) != nil }, 15*time.Second)
		second, err := owner.open(dev.ID, *session)
		if err == nil {
			defer second.conn.CloseNow()
		}
		_, elapsed, ok := waitNotification(owner, func(items []notification) bool { return findNotification(items, *session) == nil }, 15*time.Second)
		check("N4", "打开会话终端即清除提醒", err == nil && ok, elapsed.Round(100*time.Millisecond).String())
	}
	if want("N5") {
		out, _ := term.run("env -u TMUX -u TMUX_PANE "+notify+" notify hello >/dev/null 2>&1; echo rc=$?", 10*time.Second)
		check("N5", "tmux 外运行 notify 退出码 2", out == "rc=2", out)
	}
	if want("N7") {
		long := strings.Repeat("长", 170)
		term.run(notify+" notify '"+long+"'", 10*time.Second)
		items, _, ok := waitNotification(owner, func(items []notification) bool { return findNotification(items, *session) != nil }, 15*time.Second)
		n := findNotification(items, *session)
		check("N7", "超长提醒截断到 200 字节内", ok && n != nil && len(n.Message) <= 200 && strings.HasPrefix(long, n.Message), fmt.Sprint(len(n.Message)))
	}
	if want("N6") && *restartCmd != "" {
		before, _ := term.run("tmux ls -F '#{session_name}' | sort | tr '\\n' ' '", 10*time.Second)
		term.run(notify+" notify --color blue survives-restart", 10*time.Second)
		waitNotification(owner, func(items []notification) bool {
			n := findNotification(items, *session)
			return n != nil && n.Message == "survives-restart"
		}, 15*time.Second)
		_ = term.write(websocket.MessageBinary, *restartCmd+"\r")
		time.Sleep(3 * time.Second)
		term.conn.CloseNow()
		// The restarted connector rereads the notification from tmux. Check it
		// before attaching: opening the session clears it by design.
		online := false
		for i := 0; i < 30 && !online; i++ {
			time.Sleep(time.Second)
			d, err := owner.device()
			online = err == nil && d.Status == "online"
		}
		_, _, kept := waitNotification(owner, func(items []notification) bool {
			n := findNotification(items, *session)
			return n != nil && n.Message == "survives-restart"
		}, 15*time.Second)
		// Attaching to the original session only works if tmux survived.
		var reopened *terminal
		for i := 0; i < 10 && reopened == nil && online; i++ {
			reopened, _ = owner.open(dev.ID, *session)
			if reopened == nil {
				time.Sleep(time.Second)
			}
		}
		after := ""
		if reopened != nil {
			time.Sleep(800 * time.Millisecond)
			after, _ = reopened.run("tmux ls -F '#{session_name}' | sort | tr '\\n' ' '", 10*time.Second)
			defer reopened.conn.CloseNow()
			term = reopened
		}
		check("N6", "重启设备代理后 tmux 会话与提醒都保留", reopened != nil && kept && strings.TrimSpace(before) == strings.TrimSpace(after), fmt.Sprintf("notification kept=%v before=%s after=%s", kept, before, after))
	}

	// ---------- Heartbeat ----------
	if want("P1") {
		_ = term.write(websocket.MessageText, `{"type":"ping","id":"e2e-1"}`)
		got := ""
		select {
		case got = <-term.texts:
		case <-term.closed:
			got = "terminal closed"
		case <-time.After(3 * time.Second):
		}
		check("P1", "终端 ping 收到 pong", got == `{"id":"e2e-1","type":"pong"}`, got)
	}

	// ---------- Permissions ----------
	if *viewerToken != "" {
		viewer, err := login(*viewerToken)
		if err == nil {
			if want("U8") {
				status, _ := viewer.upload(dev.ID, "v.txt", []byte("x"))
				check("U8", "viewer 上传 403", status == http.StatusForbidden, fmt.Sprintf("HTTP %d", status))
			}
			if want("X5") {
				status, _, _, _ := viewer.export(dev.ID, *session, "all")
				check("X5", "viewer 导出 403", status == http.StatusForbidden, fmt.Sprintf("HTTP %d", status))
			}
		} else {
			check("--", "viewer 登录", false, err.Error())
		}
	}
}

func hostPort(address, port string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	return net.JoinHostPort(address, port)
}

func mustURL(raw string) *url.URL { parsed, _ := url.Parse(raw); return parsed }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// downloadRate measures the link in bytes per second with a static download
// from the Hub (the connector binary), or returns 0.
func downloadRate() float64 {
	client := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}}}
	start := time.Now()
	response, err := client.Get(*hub + "/downloads/sinthmux-connector-linux-amd64")
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	n, err := io.Copy(io.Discard, response.Body)
	if err != nil || response.StatusCode != http.StatusOK || n < 1<<20 {
		return 0
	}
	return float64(n) / time.Since(start).Seconds()
}

// deviceDownloadRate measures the device's link to the Hub by having the
// device download the connector binary from the Hub URL it is connected to.
func deviceDownloadRate(term *terminal) float64 {
	command := fmt.Sprintf("curl -sk -o /dev/null -w '%%{speed_download}' %s/downloads/sinthmux-connector-linux-amd64", *hub)
	out, ok := term.run(command, 90*time.Second)
	if !ok {
		return 0
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0
	}
	return rate
}
