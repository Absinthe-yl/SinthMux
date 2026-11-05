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
	"crypto/rand"
	"crypto/tls"
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
	"unicode/utf8"

	"github.com/coder/websocket"
)

var (
	hub      = flag.String("hub", "", "Hub base URL, e.g. https://106.55.15.160")
	token    = flag.String("token", os.Getenv("SINTHMUX_E2E_TOKEN"), "login token (or SINTHMUX_E2E_TOKEN)")
	device   = flag.String("device", "", "device name to test")
	session  = flag.String("session", "", "session to create; default: unique e2e-term name; existing names are refused")
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
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, *hub+path, reader)
	if err != nil {
		return 0, err
	}
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
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		if closeErr != nil {
			return response.StatusCode, fmt.Errorf("read: %v; close: %w", readErr, closeErr)
		}
		return response.StatusCode, readErr
	}
	if closeErr != nil {
		return response.StatusCode, closeErr
	}
	if len(data) > 1<<20 {
		return response.StatusCode, errors.New("HTTP response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return response.StatusCode, err
		}
	} else if out != nil {
		return response.StatusCode, errors.New("empty JSON response")
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
	conn            *websocket.Conn
	output          chan []byte
	done            chan struct{}
	stopping        chan struct{}
	cancel          context.CancelFunc
	readErr         error // published by closing output/done
	readMu          sync.Mutex
	closing         bool
	readBeforeClose bool
	err             error
	screen          screen
}

func dial(ctx context.Context, t string) (*terminal, *http.Response, error) {
	endpoint := strings.Replace(*hub, "http", "ws", 1) + "/ws/v1/terminal"
	header := http.Header{"Origin": {*hub}}
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: header, Subprotocols: []string{"sinthmux.v1", "sinthmux.ticket." + t}})
	if err != nil {
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				err = fmt.Errorf("%v; response close: %w", err, closeErr)
				response = nil // A cleanup error cannot pass the replay-rejection check.
			}
		}
		return nil, response, err
	}
	conn.SetReadLimit(1 << 20)
	readCtx, cancel := context.WithCancel(ctx)
	term := &terminal{conn: conn, output: make(chan []byte, 16), done: make(chan struct{}), stopping: make(chan struct{}), cancel: cancel}
	term.screen.init(100, 30)
	go func() {
		defer close(term.done)
		defer close(term.output)
		for {
			kind, data, err := conn.Read(readCtx)
			if err != nil {
				term.readMu.Lock()
				term.readErr = err
				term.readBeforeClose = !term.closing
				term.readMu.Unlock()
				return
			}
			if kind == websocket.MessageBinary {
				select {
				case term.output <- data:
				case <-term.stopping: // Drain reads so the close handshake cannot deadlock on a full queue.
				case <-readCtx.Done():
					term.readMu.Lock()
					term.readErr = readCtx.Err()
					term.readBeforeClose = !term.closing
					term.readMu.Unlock()
					return
				}
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
	data, err := json.Marshal(map[string]any{"type": "resize", "cols": cols, "rows": rows})
	if err != nil {
		return err
	}
	if err := t.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	t.screen.resize(cols, rows)
	return t.screen.err
}

// waitFor matches rendered terminal cells, never raw ANSI or incomplete UTF-8.
func (t *terminal) waitFor(pattern *regexp.Regexp, timeout time.Duration) ([]string, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if t.err != nil {
			return nil, false
		}
		if !t.screen.synchronized && t.screen.state == 0 && len(t.screen.pending) == 0 {
			if match := pattern.FindStringSubmatch(t.screen.text()); match != nil {
				return match, true
			}
		}
		select {
		case data, open := <-t.output:
			if !open {
				t.err = fmt.Errorf("WebSocket reader stopped: %v", t.readErr)
				return nil, false
			}
			t.screen.feed(data)
			if t.screen.err != nil {
				t.err = t.screen.err
				return nil, false
			}
		case <-deadline.C:
			t.err = errors.New("timeout waiting for rendered terminal output")
			return nil, false
		}
	}
}

var powershellPromptPattern = regexp.MustCompile(`(?m)^PS[ \t]+[^\r\n>]*>[ \t]*(?:\r?\n)?$`)

// waitForPowerShellPrompt waits for the interactive PowerShell prompt without sending input.
func (t *terminal) waitForPowerShellPrompt(timeout time.Duration) bool {
	_, ok := t.waitFor(powershellPromptPattern, timeout)
	return ok
}

func (t *terminal) close(reason string) error {
	// Only reader errors observed after local close may be expected shutdown.
	t.readMu.Lock()
	t.closing = true
	t.readMu.Unlock()
	close(t.stopping)
	err := t.conn.Close(websocket.StatusNormalClosure, reason)
	t.cancel()
	<-t.done
	if err != nil {
		return fmt.Errorf("%s: %w", reason, err)
	}
	if t.readBeforeClose || t.readErr != nil && !errors.Is(t.readErr, net.ErrClosed) && !errors.Is(t.readErr, context.Canceled) && websocket.CloseStatus(t.readErr) != websocket.StatusNormalClosure {
		return fmt.Errorf("%s: reader: %w", reason, t.readErr)
	}
	return nil
}

// Bounded parser state survives WebSocket message boundaries.
type screen struct {
	cells                     [][]rune
	history                   []string
	x, y, sx, sy, top, bottom int
	state                     byte
	sequence                  []byte
	pending                   []byte
	controlBytes              int
	err                       error
	wrap, origin, insert      bool
	synchronized              bool
	last                      rune
	primary                   [][]rune
	primaryX, primaryY        int
}

func (s *screen) init(cols, rows int) {
	if cols < 1 || cols > 512 || rows < 1 || rows > 128 {
		s.err = errors.New("screen dimensions outside bounds")
		return
	}
	s.cells = make([][]rune, rows)
	for i := range s.cells {
		s.cells[i] = make([]rune, cols)
	}
	s.history = nil
	s.x, s.y, s.top, s.bottom = 0, 0, 0, rows-1
	s.sx, s.sy = 0, 0
	s.wrap, s.origin, s.insert = true, false, false
	s.synchronized = false
	s.primary = nil
}

// Resize must preserve partial ANSI/UTF-8 and terminal modes.
func (s *screen) resize(cols, rows int) {
	if cols < 2 || cols > 512 || rows < 1 || rows > 128 {
		s.err = errors.New("screen dimensions outside bounds")
		return
	}
	resizeCells := func(old [][]rune) [][]rune {
		if old == nil {
			return nil
		}
		cells := make([][]rune, rows)
		for y := range cells {
			cells[y] = make([]rune, cols)
			if y < len(old) {
				copy(cells[y], old[y])
			}
		}
		return cells
	}
	s.cells, s.primary = resizeCells(s.cells), resizeCells(s.primary)
	s.top, s.bottom = 0, rows-1
	s.clamp()
	if s.sx >= cols {
		s.sx = cols - 1
	}
	if s.sy >= rows {
		s.sy = rows - 1
	}
	if s.primaryX >= cols {
		s.primaryX = cols - 1
	}
	if s.primaryY >= rows {
		s.primaryY = rows - 1
	}
}

func cellText(row []rune) string {
	var b strings.Builder
	for _, r := range row {
		if r == -1 {
			continue
		} // continuation cell of a wide character
		if r == 0 {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

func (s *screen) text() string {
	lines := []string(nil)
	if s.primary == nil {
		lines = append(lines, s.history...)
	}
	for _, row := range s.cells {
		lines = append(lines, cellText(row))
	}
	return strings.Join(lines, "\n")
}

func (s *screen) scroll() {
	if s.primary == nil && s.top == 0 && s.bottom == len(s.cells)-1 {
		if len(s.history) == 256 {
			copy(s.history, s.history[1:])
			s.history = s.history[:255]
		}
		s.history = append(s.history, cellText(s.cells[0]))
	}
	copy(s.cells[s.top:s.bottom], s.cells[s.top+1:s.bottom+1])
	s.cells[s.bottom] = make([]rune, len(s.cells[0]))
}

func (s *screen) newline() {
	if s.y == s.bottom {
		s.scroll()
	} else if s.y < len(s.cells)-1 {
		s.y++
	}
}

func runeWidth(r rune) int {
	if r >= 0x300 && r <= 0x36f || r >= 0xfe00 && r <= 0xfe0f {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff || r >= 0x20000) {
		return 2
	}
	return 1
}

func (s *screen) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return
	}
	cols := len(s.cells[0])
	if w > cols {
		s.err = errors.New("wide character cannot fit terminal width")
		return
	}
	if s.x+w > cols {
		if s.wrap {
			s.x = 0
			s.newline()
		} else {
			s.x = cols - w
		}
	}
	if s.insert {
		copy(s.cells[s.y][s.x+w:], s.cells[s.y][s.x:cols-w])
	}
	// Erase both halves if a cursor write intersects an old wide cell.
	for i := s.x; i < s.x+w; i++ {
		if s.cells[s.y][i] == -1 && i > 0 {
			s.cells[s.y][i-1] = 0
		}
		if i+1 < cols && s.cells[s.y][i+1] == -1 {
			s.cells[s.y][i+1] = 0
		}
		s.cells[s.y][i] = 0
	}
	s.cells[s.y][s.x] = r
	if w == 2 {
		s.cells[s.y][s.x+1] = -1
	}
	s.x += w
	s.last = r
}

func (s *screen) feed(data []byte) {
	for _, b := range data {
		if s.err != nil {
			return
		}
		if s.state != 0 {
			s.controlBytes++
			if s.controlBytes > 8192 {
				s.err = errors.New("terminal control sequence exceeds bound")
				return
			}
			switch s.state {
			case 'e':
				switch b {
				case '[':
					s.state = 'c'
					s.sequence = s.sequence[:0]
				case ']', 'P', '^', '_':
					s.state = 'o'
				case '(', ')', '*', '+':
					s.state = 'g'
				case '7':
					s.sx, s.sy, s.state = s.x, s.y, 0
				case '8':
					s.x, s.y, s.state = s.sx, s.sy, 0
					s.clamp()
				case 'D':
					s.newline()
					s.state = 0
				case 'E':
					s.x = 0
					s.newline()
					s.state = 0
				case 'M':
					if s.y == s.top {
						copy(s.cells[s.top+1:s.bottom+1], s.cells[s.top:s.bottom])
						s.cells[s.top] = make([]rune, len(s.cells[0]))
					} else if s.y > 0 {
						s.y--
					}
					s.state = 0
				case 'c':
					s.init(len(s.cells[0]), len(s.cells))
					s.state = 0
				case '=', '>':
					s.state = 0
				default:
					s.err = fmt.Errorf("unsupported terminal ESC %q", b)
				}
			case 'g':
				s.state = 0
			case 'o':
				if b == 7 {
					s.state = 0
				} else if b == 27 {
					s.state = 't'
				}
			case 't':
				if b == '\\' {
					s.state = 0
				} else {
					s.state = 'o'
				}
			case 'c':
				if b >= 0x40 && b <= 0x7e {
					s.csi(b)
					s.state = 0
				} else {
					if len(s.sequence) == 128 {
						s.err = errors.New("CSI parameters exceed bound")
						return
					}
					s.sequence = append(s.sequence, b)
				}
			}
			continue
		}
		if len(s.pending) > 0 || b >= 0x80 {
			s.pending = append(s.pending, b)
			if !utf8.FullRune(s.pending) {
				continue
			}
			r, n := utf8.DecodeRune(s.pending)
			if r == utf8.RuneError && n == 1 {
				s.err = errors.New("invalid terminal UTF-8")
				return
			}
			s.pending = s.pending[:0]
			s.put(r)
			continue
		}
		switch b {
		case 27:
			s.state = 'e'
			s.controlBytes = 0
		case '\r':
			s.x = 0
		case '\n', '\v', '\f':
			s.newline()
		case '\b':
			if s.x > 0 {
				s.x--
			}
		case '\t':
			s.x = (s.x/8 + 1) * 8
			if s.x >= len(s.cells[0]) {
				s.x = len(s.cells[0]) - 1
			}
		default:
			if b >= 32 && b != 127 {
				s.put(rune(b))
			}
		}
	}
}

func (s *screen) clamp() {
	if s.x < 0 {
		s.x = 0
	}
	if s.x >= len(s.cells[0]) {
		s.x = len(s.cells[0]) - 1
	}
	if s.y < 0 {
		s.y = 0
	}
	if s.y >= len(s.cells) {
		s.y = len(s.cells) - 1
	}
}

func (s *screen) csi(final byte) {
	raw := string(s.sequence)
	private := strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, ">") || strings.HasPrefix(raw, "!")
	raw = strings.TrimLeft(raw, "?>!")
	parts := strings.Split(raw, ";")
	n := func(i, fallback int) int {
		if i >= len(parts) || parts[i] == "" {
			return fallback
		}
		v, err := strconv.Atoi(parts[i])
		if err != nil || v < 0 || v > 65535 {
			s.err = errors.New("invalid CSI parameter")
			return fallback
		}
		if v == 0 && fallback == 1 {
			return 1
		}
		return v
	}
	cols := len(s.cells[0])
	if private && final != 'h' && final != 'l' && final != 'c' && final != 'n' && !(strings.HasPrefix(string(s.sequence), ">") && final == 'm') {
		s.err = errors.New("unsupported private CSI")
		return
	}
	switch final {
	case 'm', 'n', 'c', 'q': // appearance and reports; no printable content
	case 'h', 'l':
		for i := range parts {
			mode, enabled := n(i, 0), final == 'h'
			if !private {
				if mode == 4 {
					s.insert = enabled
				} else {
					s.err = fmt.Errorf("unsupported terminal mode %d", mode)
				}
				continue
			}
			switch mode {
			case 6:
				s.origin = enabled
				s.x, s.y = 0, 0
				if enabled {
					s.y = s.top
				}
			case 7:
				s.wrap = enabled
			case 47, 1047, 1049:
				if enabled && s.primary == nil {
					s.primary, s.primaryX, s.primaryY = s.cells, s.x, s.y
					s.cells = make([][]rune, len(s.primary))
					for y := range s.cells {
						s.cells[y] = make([]rune, cols)
					}
					s.x, s.y = 0, 0
				} else if !enabled && s.primary != nil {
					s.cells, s.x, s.y, s.primary = s.primary, s.primaryX, s.primaryY, nil
				}
			case 1048:
				if enabled {
					s.sx, s.sy = s.x, s.y
				} else {
					s.x, s.y = s.sx, s.sy
					s.clamp()
				}
			case 2026:
				s.synchronized = enabled
			case 9001: // ConPTY win32-input-mode does not change output cells.
			case 1, 12, 25, 1000, 1002, 1003, 1004, 1005, 1006, 1007, 1015, 2004: // keyboard, cursor, mouse, paste
			default:
				s.err = fmt.Errorf("unsupported private terminal mode %d", mode)
			}
		}
	case 'A':
		s.y -= n(0, 1)
	case 'B', 'e':
		s.y += n(0, 1)
	case 'C', 'a':
		s.x += n(0, 1)
	case 'D':
		s.x -= n(0, 1)
	case 'E':
		s.y += n(0, 1)
		s.x = 0
	case 'F':
		s.y -= n(0, 1)
		s.x = 0
	case 'G', '`':
		s.x = n(0, 1) - 1
	case 'd':
		s.y = n(0, 1) - 1
		if s.origin {
			s.y += s.top
		}
	case 'H', 'f':
		s.y, s.x = n(0, 1)-1, n(1, 1)-1
		if s.origin {
			s.y += s.top
		}
	case 's':
		s.sx, s.sy = s.x, s.y
	case 'u':
		s.x, s.y = s.sx, s.sy
	case 'r':
		s.top, s.bottom = n(0, 1)-1, n(1, len(s.cells))-1
		if s.top < 0 || s.bottom >= len(s.cells) || s.top >= s.bottom {
			s.err = errors.New("invalid scroll region")
			return
		}
		s.x, s.y = 0, 0
	case 'J', 'K', 'X':
		mode := n(0, 0)
		for y := range s.cells {
			for x := range s.cells[y] {
				erase := final == 'J' && (mode == 2 || mode == 3 || mode == 0 && (y > s.y || y == s.y && x >= s.x) || mode == 1 && (y < s.y || y == s.y && x <= s.x)) || final == 'K' && y == s.y && (mode == 2 || mode == 0 && x >= s.x || mode == 1 && x <= s.x) || final == 'X' && y == s.y && x >= s.x && x-s.x < n(0, 1)
				if erase {
					s.cells[y][x] = 0
				}
			}
		}
		if final == 'J' && mode == 3 {
			s.history = nil
		}
	case 'P', '@':
		count := n(0, 1)
		if count > cols-s.x {
			count = cols - s.x
		}
		if count < 0 {
			count = 0
		}
		row := s.cells[s.y]
		if final == 'P' {
			copy(row[s.x:], row[s.x+count:])
			for i := cols - count; i < cols; i++ {
				row[i] = 0
			}
		} else {
			copy(row[s.x+count:], row[s.x:cols-count])
			for i := s.x; i < s.x+count; i++ {
				row[i] = 0
			}
		}
	case 'L', 'M', 'S', 'T':
		count := n(0, 1)
		if count > len(s.cells) {
			count = len(s.cells)
		}
		top := s.top
		if final == 'L' || final == 'M' {
			top = s.y
		}
		if top < s.top || top > s.bottom {
			break
		}
		for i := 0; i < count; i++ {
			if final == 'S' {
				s.scroll()
			} else if final == 'M' {
				copy(s.cells[top:s.bottom], s.cells[top+1:s.bottom+1])
				s.cells[s.bottom] = make([]rune, cols)
			} else {
				copy(s.cells[top+1:s.bottom+1], s.cells[top:s.bottom])
				s.cells[top] = make([]rune, cols)
			}
		}
	case 'b':
		count := n(0, 1)
		if count > 512*128 {
			s.err = errors.New("repeat exceeds screen bound")
			return
		}
		if s.last != 0 {
			for i := 0; i < count; i++ {
				s.put(s.last)
			}
		}
	case 't':
		if op := n(0, 0); op != 22 && op != 23 {
			s.err = fmt.Errorf("unsupported window operation %d", op)
		}
	default:
		s.err = fmt.Errorf("unsupported CSI final %q", final)
	}
	// Preserve delayed autowrap for appearance-only sequences after the last cell.
	if final != 'm' && final != 'h' && final != 'l' {
		s.clamp()
	}
	if s.origin {
		if s.y < s.top {
			s.y = s.top
		}
		if s.y > s.bottom {
			s.y = s.bottom
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
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	marker := fmt.Sprintf("%x", nonce[:])
	if *session == "" {
		*session = "e2e-term-" + marker
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
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
	check("登录令牌登录", err == nil && status == http.StatusOK, httpDetail(status, err))
	if err != nil || status != http.StatusOK {
		return
	}
	defer func() {
		status, err := call(http.MethodPost, "/api/v1/auth/logout", nil, nil)
		check("退出登录", err == nil && (status == http.StatusOK || status == http.StatusNoContent), httpDetail(status, err))
	}()
	var me struct{ CSRF string }
	status, err = call(http.MethodGet, "/api/v1/auth/me", nil, &me)
	check("读取登录状态及 CSRF", err == nil && status == http.StatusOK && me.CSRF != "", httpDetail(status, err))
	if err != nil || status != http.StatusOK || me.CSRF == "" {
		return
	}
	csrf = me.CSRF

	var list struct {
		Devices []struct{ ID, Name, Status, Platform string }
	}
	var deviceID, platform string
	for i := 0; i < 20 && deviceID == ""; i++ {
		list.Devices = nil
		status, err = call(http.MethodGet, "/api/v1/devices", nil, &list)
		if err != nil || status != http.StatusOK {
			check("读取设备列表", false, httpDetail(status, err))
			return
		}
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
	var sessions struct {
		Sessions []struct {
			Name    string
			Windows int
		}
	}
	status, err = call(http.MethodGet, base, nil, &sessions)
	if err != nil || status != http.StatusOK {
		check("创建前读取会话列表", false, httpDetail(status, err))
		return
	}
	for _, s := range sessions.Sessions {
		if s.Name == *session {
			check("测试会话名称无冲突", false, "会话已存在，停止且不删除: "+*session)
			return
		}
	}
	status, err = call(http.MethodPost, base, map[string]string{"name": *session}, nil)
	check("创建 tmux 会话", err == nil && status == http.StatusCreated, httpDetail(status, err)+" / "+*session)
	if err != nil || status != http.StatusCreated {
		return
	}
	// Only a successful create grants this run ownership for cleanup.
	if !*keep {
		defer func() {
			status, err := call(http.MethodDelete, base+"/"+url.PathEscape(*session), nil, nil)
			check("关闭本次创建的会话", err == nil && (status == http.StatusNoContent || status == http.StatusOK), httpDetail(status, err))
		}()
	}
	sessions.Sessions = nil
	status, err = call(http.MethodGet, base, nil, &sessions)
	if err != nil || status != http.StatusOK {
		check("读取会话列表", false, httpDetail(status, err))
		return
	}
	found := false
	for _, s := range sessions.Sessions {
		found = found || (s.Name == *session && s.Windows >= 1)
	}
	check("会话出现在列表中", found, fmt.Sprintf("%d 个会话", len(sessions.Sessions)))
	if !found {
		return
	}

	first, err := ticket(deviceID)
	check("签发终端票据", err == nil, errString(err))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	term, _, err := dial(ctx, first)
	check("终端 WebSocket 连接", err == nil, errString(err))
	if err != nil {
		return
	}
	closed := false
	defer func() {
		if !closed {
			check("关闭首个终端", closeOK(term, "cleanup"), errString(term.err))
		}
	}()
	windows := strings.EqualFold(platform, "windows")
	if err := term.resize(100, 30); err != nil {
		check("初始化终端尺寸", false, errString(err))
		return
	}
	if windows {
		check("等待 PowerShell 提示符", term.waitForPowerShellPrompt(10*time.Second), errString(term.err))
		if term.err != nil {
			return
		}
	}
	command := "printf '%s-%s\\n' '" + marker + "' \"$((6*7))\"\r"
	if windows {
		command = "Write-Output ('" + marker + "'+'-'+(6*7))\r"
	}
	if !runCommand(term, "输入命令并收到计算结果", command, linePattern(marker+"-42")) {
		return
	}
	if err := term.resize(123, 37); err != nil {
		check("发送窗口尺寸", false, errString(err))
		return
	}
	command = "z=$(stty size) && printf '%s-%s:END\\n' 'SIZE" + marker + "' \"$z\"\r"
	if windows {
		command = "Write-Output ('SIZE" + marker + "'+'-'+$Host.UI.RawUI.WindowSize.Height+' '+$Host.UI.RawUI.WindowSize.Width+':END')\r"
	}
	// The psmux/tmux status bar consumes one of the 37 rows.
	if !runCommand(term, "窗口尺寸：123 列、36 行（状态栏占 1 行）", command, linePattern("SIZE"+marker+"-36 123:END")) {
		return
	}
	command = "u='中文'; printf 'I-%s-%s\\n' \"$u\" '" + marker + "'\r"
	if windows {
		command = "$u='中文';Write-Output ('I-'+$u+'-" + marker + "')\r"
	}
	if err := term.send(command); err != nil {
		check("发送中文输入", false, errString(err))
		return
	}
	_, ok := term.waitFor(regexp.MustCompile(regexp.QuoteMeta("'中文'")), 10*time.Second)
	check("UTF-8 中文输入回显", ok, errString(term.err))
	if !ok {
		return
	}
	_, ok = term.waitFor(linePattern("I-中文-"+marker), 10*time.Second)
	check("UTF-8 中文输入经 shell 处理", ok, errString(term.err))
	if !ok {
		return
	}
	// The output probe contains no literal Chinese: echo cannot satisfy it.
	command = "printf 'O-\\344\\270\\255\\346\\226\\207-%s\\n' '" + marker + "'\r"
	if windows {
		command = "Write-Output ('O-'+[string][char]0x4e2d+[char]0x6587+'-" + marker + "')\r"
	}
	if !runCommand(term, "UTF-8 中文输出", command, linePattern("O-中文-"+marker)) {
		return
	}
	// Bound elapsed time using local send/READY times, without clock synchronization.
	command = "s=$(date +%s); printf '%s%s\\n' 'READY-' '" + marker + "'; sleep 2; printf '%s%s:%s:END\\n' 'BACKGROUND-' '" + marker + "' \"$((($(date +%s)-s)*1000))\"\r"
	if windows {
		command = "$sw=[Diagnostics.Stopwatch]::StartNew();Write-Output ('READY-'+'" + marker + "');Start-Sleep -Seconds 2;Write-Output ('BACKGROUND-'+'" + marker + ":'+$sw.ElapsedMilliseconds+':END')\r"
	}
	sentAt := time.Now()
	if !runCommand(term, "收到真实 READY 输出", command, linePattern("READY-"+marker)) {
		return
	}
	readyAt := time.Now()
	closeErr := term.close("browser disconnected")
	closed = true
	disconnectedAt := time.Now()
	check("断开首个终端", closeErr == nil, errString(closeErr))
	if closeErr != nil {
		return
	}
	// BACKGROUND's timestamp must fall inside the measured detached window.
	detachedWindow := 3 * time.Second
	if !windows {
		detachedWindow = 4 * time.Second
	} // Allow for date's coarse timestamp bounds.
	window := time.NewTimer(time.Until(readyAt.Add(detachedWindow)))
	select {
	case <-window.C:
	case <-ctx.Done():
		window.Stop()
		check("断开观察窗口", false, errString(ctx.Err()))
		return
	}
	replay, response, err := dial(ctx, first)
	rejected := err != nil && response != nil && response.StatusCode == http.StatusUnauthorized
	if replay != nil {
		check("关闭意外重放连接", closeOK(replay, "unexpected replay"), errString(replay.err))
	}
	replayDetail := errString(err)
	if response != nil {
		replayDetail = httpDetail(response.StatusCode, err)
	}
	check("同一票据不能重复使用", rejected, replayDetail)
	if !rejected {
		return
	}

	second, err := ticket(deviceID)
	check("重新签发票据", err == nil, errString(err))
	if err != nil {
		return
	}
	reattachAt := time.Now()
	again, _, err := dial(ctx, second)
	check("断开后重新进入同一会话", err == nil, errString(err))
	if err != nil {
		return
	}
	again.screen.init(123, 37)
	defer func() { check("关闭重连终端", closeOK(again, "done"), errString(again.err)) }()
	// Keep the same size: a resize could clear or alter the screen being checked.
	match, ok := again.waitFor(regexp.MustCompile(`(?m)^BACKGROUND-`+regexp.QuoteMeta(marker)+`:(\d+):END *$`), 10*time.Second)
	detail := errString(again.err)
	if ok {
		elapsed, parseErr := strconv.ParseInt(match[1], 10, 64)
		uncertainty := int64(1) // Stopwatch rounds to milliseconds.
		if !windows {
			uncertainty = 1000
		} // date +%s has one-second precision.
		lower := disconnectedAt.Sub(sentAt).Milliseconds()
		upper := reattachAt.Sub(readyAt).Milliseconds()
		// Slow ticket/HTTP requests must not extend the allowed production window.
		if upper > detachedWindow.Milliseconds() {
			upper = detachedWindow.Milliseconds()
		}
		ok = parseErr == nil && elapsed > lower+uncertainty && elapsed < upper-uncertainty
		detail = fmt.Sprintf("elapsed=%dms; detached bounds=(%d,%d)ms; uncertainty=%dms", elapsed, lower, upper, uncertainty)
	}
	check("断开期间产生的输出在重入时可见", ok, detail)
}

func linePattern(text string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(text) + ` *$`)
}

func runCommand(t *terminal, name, command string, pattern *regexp.Regexp) bool {
	if err := t.send(command); err != nil {
		check(name, false, errString(err))
		return false
	}
	_, ok := t.waitFor(pattern, 10*time.Second)
	check(name, ok, errString(t.err))
	return ok
}

func closeOK(t *terminal, reason string) bool {
	t.err = t.close(reason)
	return t.err == nil
}

func httpDetail(status int, err error) string {
	return fmt.Sprintf("HTTP %d %s", status, errString(err))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	var close websocket.CloseError
	if errors.As(err, &close) {
		return fmt.Sprintf("WebSocket close %d: %s", close.Code, close.Reason)
	}
	return err.Error()
}
